package service

import (
	"atlas-render/internal/bundle"
	"atlas-render/internal/model"
	"atlas-render/internal/scene"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Decisions for a conflicting package asset.
const (
	DecisionKeep  = "keep"  // keep the target asset untouched, imported document references the target id
	DecisionAdopt = "adopt" // take the package bytes under a fresh asset id, imported document is rewritten
)

// ImportAssetPreview describes one package asset against the current target.
type ImportAssetPreview struct {
	AssetID       string `json:"assetId"`
	Status        string `json:"status"`
	Reason        string `json:"reason,omitempty"`
	PackageDigest string `json:"packageDigest"`
	PackageWidth  int    `json:"packageWidth"`
	PackageHeight int    `json:"packageHeight"`
	TargetDigest  string `json:"targetDigest,omitempty"`
	TargetState   string `json:"targetState,omitempty"`
	TargetWidth   int    `json:"targetWidth,omitempty"`
	TargetHeight  int    `json:"targetHeight,omitempty"`
	// Allowed lists the decisions the confirm step accepts for this asset.
	Allowed []string `json:"allowed,omitempty"`
}

// ImportPreview is the human-understandable, read-only view returned before a commit.
type ImportPreview struct {
	DestinationID  string               `json:"destinationId"`
	Name           string               `json:"name"`
	SnapshotDigest string               `json:"snapshotDigest"`
	SceneStatus    string               `json:"sceneStatus"`
	SceneReason    string               `json:"sceneReason,omitempty"`
	Assets         []ImportAssetPreview `json:"assets"`
	Summary        map[string]int       `json:"summary"`
	// Fingerprint pins the target scene/asset facts observed while previewing.
	// Confirm recomputes it inside the write transaction; a stale preview is rejected.
	Fingerprint string `json:"fingerprint"`
}

// ImportResult is the committed outcome of a confirmed import.
type ImportResult struct {
	Scene          model.Scene       `json:"scene"`
	AddedAssets    []string          `json:"addedAssets"`
	ReusedAssets   []string          `json:"reusedAssets"`
	RemappedAssets map[string]string `json:"remappedAssets,omitempty"`
	ResolvedAssets map[string]string `json:"resolvedAssets"`
}

// PreviewConflictError carries the preview alongside a 409 so callers can decide.
type PreviewConflictError struct {
	Message string
	Preview ImportPreview
}

func (e *PreviewConflictError) Error() string { return e.Message }

// importFingerprint captures every target fact the confirm step depends on.
// A change to the destination scene or to any referenced package asset invalidates it.
func importFingerprint(st *model.State, destID, snapshotDigest string, assets map[string]model.Asset) string {
	ids := make([]string, 0, len(assets))
	for id := range assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	type fact struct {
		Exists bool
		Digest string
		State  string
		Width  int
		Height int
	}
	targets := map[string]fact{}
	for _, id := range ids {
		if a, ok := st.Assets[id]; ok {
			targets[id] = fact{true, a.Digest, a.State, a.Width, a.Height}
		} else {
			targets[id] = fact{}
		}
	}
	sc, sceneExists := st.Scenes[destID]
	return model.CanonicalDigest(struct {
		Destination    string
		SnapshotDigest string
		SceneExists    bool
		SceneRevision  int
		SceneState     string
		SceneDigest    string
		Targets        map[string]fact
	}{destID, snapshotDigest, sceneExists, sc.Revision, sc.State, model.CanonicalDigest(sc.Document), targets})
}

func buildImportPreview(st *model.State, destID, name string, p bundle.Package) ImportPreview {
	assets := p.Snapshot.Assets
	ids := make([]string, 0, len(assets))
	for id := range assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	prev := ImportPreview{
		DestinationID:  destID,
		Name:           name,
		SnapshotDigest: p.Snapshot.Digest,
		SceneStatus:    "free",
		Assets:         []ImportAssetPreview{},
		Summary:        map[string]int{"new": 0, "identical": 0, "contentConflict": 0, "retiredConflict": 0},
	}
	if _, ok := st.Scenes[destID]; ok {
		prev.SceneStatus = "conflict"
		prev.SceneReason = "destination scene already exists"
	}

	for _, id := range ids {
		a := assets[id]
		row := ImportAssetPreview{
			AssetID:       id,
			PackageDigest: a.Digest,
			PackageWidth:  a.Width,
			PackageHeight: a.Height,
		}
		existing, ok := st.Assets[id]
		switch {
		case !ok:
			row.Status = "new"
			row.Reason = "no target asset with this id"
			prev.Summary["new"]++
		case existing.Digest == a.Digest && existing.State == "available":
			row.Status = "identical"
			row.Reason = "target asset has identical bytes and is available"
			row.TargetDigest, row.TargetState = existing.Digest, existing.State
			row.TargetWidth, row.TargetHeight = existing.Width, existing.Height
			prev.Summary["identical"]++
		case existing.State != "available":
			row.Status = "retiredConflict"
			row.Reason = "target asset is retired and cannot be reused or replaced"
			row.TargetDigest, row.TargetState = existing.Digest, existing.State
			row.TargetWidth, row.TargetHeight = existing.Width, existing.Height
			row.Allowed = []string{DecisionAdopt}
			prev.Summary["retiredConflict"]++
		default:
			row.Status = "contentConflict"
			row.Reason = "target asset id exists with different bytes"
			row.TargetDigest, row.TargetState = existing.Digest, existing.State
			row.TargetWidth, row.TargetHeight = existing.Width, existing.Height
			row.Allowed = []string{DecisionKeep, DecisionAdopt}
			prev.Summary["contentConflict"]++
		}
		prev.Assets = append(prev.Assets, row)
	}
	prev.Fingerprint = importFingerprint(st, destID, p.Snapshot.Digest, assets)
	return prev
}

func previewHasBlockingConflict(prev ImportPreview) bool {
	if prev.SceneStatus != "free" {
		return true
	}
	for _, a := range prev.Assets {
		if a.Status == "contentConflict" || a.Status == "retiredConflict" {
			return true
		}
	}
	return false
}

// ImportPreview validates the package and computes a read-only conflict preview.
// It never mutates the target state.
func (s *Service) ImportPreview(id, name string, p bundle.Package) (ImportPreview, error) {
	var prev ImportPreview
	if !scene.ValidID(id) || strings.TrimSpace(name) == "" || len(name) > 160 {
		return prev, model.Invalid("invalid import destination")
	}
	if e := bundle.Validate(p); e != nil {
		return prev, e
	}
	snapshot := s.Store.Read()
	prev = buildImportPreview(&snapshot, id, name, p)
	return prev, nil
}

// uniqueAssetID derives a fresh, valid asset id that collides with neither the
// committed state nor the assets already remapped within this transaction.
func uniqueAssetID(st *model.State, taken map[string]bool, base, suffix string) string {
	const maxLen = 80
	trim := func(b, tail string) string {
		if len(b)+len(tail) > maxLen {
			b = b[:maxLen-len(tail)]
		}
		return b + tail
	}
	candidate := trim(base, "-"+suffix)
	for n := 2; ; n++ {
		if _, used := st.Assets[candidate]; !used && !taken[candidate] && scene.ValidID(candidate) {
			taken[candidate] = true
			return candidate
		}
		tail := fmt.Sprintf("-%s-%d", suffix, n)
		candidate = trim(base, tail)
	}
}

// remapAssetIDs rewrites every image layer reference from old to new.
func remapAssetIDs(ls []model.Layer, mapping map[string]string) {
	for i := range ls {
		if ls[i].Kind == "image" {
			if next, ok := mapping[ls[i].AssetID]; ok {
				ls[i].AssetID = next
			}
		}
		remapAssetIDs(ls[i].Children, mapping)
	}
}

// ImportConfirm commits a previewed import. All checks run inside the single
// store transaction so a failed or stale confirmation leaves the target unchanged.
func (s *Service) ImportConfirm(id, name, fingerprint string, p bundle.Package, decisions map[string]string) (ImportResult, error) {
	var out ImportResult
	if !scene.ValidID(id) || strings.TrimSpace(name) == "" || len(name) > 160 {
		return out, model.Invalid("invalid import destination")
	}
	if fingerprint == "" {
		return out, model.Invalid("import preview fingerprint required")
	}
	if e := bundle.Validate(p); e != nil {
		return out, e
	}
	e := s.Store.Update(func(st *model.State) error {
		// Re-evaluate against the live state; reject any stale preview.
		current := buildImportPreview(st, id, name, p)
		if current.Fingerprint != fingerprint {
			return model.Conflict("import preview is stale: target scene or assets changed")
		}
		if current.SceneStatus != "free" {
			return &PreviewConflictError{Message: "destination scene already exists", Preview: current}
		}

		previewByID := map[string]ImportAssetPreview{}
		for _, row := range current.Assets {
			previewByID[row.AssetID] = row
		}
		// Every conflicting asset needs an explicit, allowed decision.
		for _, row := range current.Assets {
			if row.Status != "contentConflict" && row.Status != "retiredConflict" {
				continue
			}
			decision, ok := decisions[row.AssetID]
			if !ok {
				return model.Invalid("missing decision for conflicting asset %s", row.AssetID)
			}
			allowed := false
			for _, a := range row.Allowed {
				if a == decision {
					allowed = true
					break
				}
			}
			if !allowed {
				return model.Invalid("decision %q is not allowed for asset %s", decision, row.AssetID)
			}
		}
		// Reject decisions on assets that do not actually conflict (stale/garbage input).
		for aid := range decisions {
			row, ok := previewByID[aid]
			if !ok {
				return model.Invalid("unknown package asset %s", aid)
			}
			if row.Status != "contentConflict" && row.Status != "retiredConflict" {
				return model.Invalid("asset %s does not require a decision", aid)
			}
		}

		doc := model.Clone(p.Snapshot.Document)
		mapping := map[string]string{}
		remapped := map[string]string{}
		resolved := map[string]string{}
		added := []string{}
		reused := []string{}
		taken := map[string]bool{}

		for _, row := range current.Assets {
			pkgAsset := p.Snapshot.Assets[row.AssetID]
			switch row.Status {
			case "new":
				// Fresh id, package bytes land directly.
				st.Assets[row.AssetID] = pkgAsset
				added = append(added, row.AssetID)
				resolved[row.AssetID] = row.AssetID
			case "identical":
				// Shared, identical and available: keep the target record untouched.
				if st.Assets[row.AssetID].State != "available" {
					return model.Conflict(fmt.Sprintf("target asset %s is no longer available", row.AssetID))
				}
				reused = append(reused, row.AssetID)
				resolved[row.AssetID] = row.AssetID
			case "contentConflict":
				switch decisions[row.AssetID] {
				case DecisionKeep:
					// Preserve the target asset; the imported scene references its id.
					if st.Assets[row.AssetID].State != "available" {
						return model.Conflict(fmt.Sprintf("target asset %s is no longer available", row.AssetID))
					}
					reused = append(reused, row.AssetID)
					resolved[row.AssetID] = row.AssetID
				case DecisionAdopt:
					// Package bytes enter under a new id; target asset and every frozen
					// render keep their original id and bytes.
					newID := uniqueAssetID(st, taken, row.AssetID, "imported")
					imported := model.Clone(pkgAsset)
					imported.ID = newID
					imported.State = "available"
					st.Assets[newID] = imported
					mapping[row.AssetID] = newID
					remapped[row.AssetID] = newID
					added = append(added, newID)
					resolved[row.AssetID] = newID
				}
			case "retiredConflict":
				// Retired target can never be reused or renamed; package bytes must
				// enter under a fresh id.
				newID := uniqueAssetID(st, taken, row.AssetID, "imported")
				imported := model.Clone(pkgAsset)
				imported.ID = newID
				imported.State = "available"
				st.Assets[newID] = imported
				mapping[row.AssetID] = newID
				remapped[row.AssetID] = newID
				added = append(added, newID)
				resolved[row.AssetID] = newID
			}
		}
		if len(mapping) > 0 {
			remapAssetIDs(doc.Layers, mapping)
		}
		if e := scene.Validate(doc); e != nil {
			return e
		}
		for _, ref := range scene.References(doc) {
			a, ok := st.Assets[ref]
			if !ok || a.State != "available" {
				return model.Conflict(fmt.Sprintf("imported scene references unavailable asset %s", ref))
			}
		}

		out.Scene = model.Scene{ID: id, Name: name, Revision: 1, State: "active", Document: doc, UpdatedAt: time.Now().UTC()}
		st.Scenes[id] = out.Scene
		st.Emit("scene.imported", id)
		sort.Strings(added)
		sort.Strings(reused)
		out.AddedAssets = added
		out.ReusedAssets = reused
		if len(remapped) > 0 {
			out.RemappedAssets = remapped
		}
		out.ResolvedAssets = resolved
		return nil
	})
	return out, e
}

// Import keeps the original one-shot, all-or-nothing entry point. Packages with
// no conflicts commit directly; conflicting packages return 409 with a preview.
func (s *Service) Import(id, name string, p bundle.Package) (model.Scene, error) {
	prev, e := s.ImportPreview(id, name, p)
	if e != nil {
		return model.Scene{}, e
	}
	if previewHasBlockingConflict(prev) {
		return model.Scene{}, &PreviewConflictError{Message: "import conflicts with destination data", Preview: prev}
	}
	res, e := s.ImportConfirm(id, name, prev.Fingerprint, p, map[string]string{})
	if e != nil {
		return model.Scene{}, e
	}
	return res.Scene, nil
}

// Export is retained alongside the two-phase flow.
func (s *Service) Export(id string) (bundle.Package, error) {
	v, ok := s.Store.Read().Snapshots[id]
	if !ok {
		return bundle.Package{}, model.Missing("snapshot not found")
	}
	return bundle.Export(v), nil
}
