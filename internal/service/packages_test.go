package service

import (
	"atlas-render/internal/bundle"
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func pngBytes(c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if e := png.Encode(&b, img); e != nil {
		panic(e)
	}
	return b.Bytes()
}

func assetRecord(id string, data []byte) model.Asset {
	return model.Asset{ID: id, Digest: model.Digest(data), State: "available", Width: 2, Height: 2, PNG: data}
}

func imageDoc(ids ...string) model.Document {
	ls := make([]model.Layer, 0, len(ids))
	for i, id := range ids {
		ls = append(ls, model.Layer{
			ID: id + "-layer", Kind: "image", Width: 40, Height: 40, AssetID: id, Opacity: 1,
			Transform: model.Transform{X: float64(10 + i*50), Y: 10, ScaleX: 1, ScaleY: 1},
		})
	}
	return model.Document{Width: 320, Height: 200, Background: "#202830", Layers: ls}
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	store, e := persist.Open(filepath.Join(t.TempDir(), "state.json"))
	if e != nil {
		t.Fatalf("open store: %v", e)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &Service{Store: store}
}

// buildPackage constructs a valid exchange package carrying the given document
// and asset bytes.
func buildPackage(t *testing.T, doc model.Document, assets map[string][]byte) bundle.Package {
	t.Helper()
	am := map[string]model.Asset{}
	for id, data := range assets {
		am[id] = assetRecord(id, data)
	}
	snap := model.Snapshot{
		ID: "snap-src", SceneID: "src", Revision: 1, State: "active",
		Document: doc, Assets: am, CreatedAt: time.Now().UTC(),
	}
	snap.Digest = model.SnapshotDigest(snap)
	p := bundle.Export(snap)
	if e := bundle.Validate(p); e != nil {
		t.Fatalf("package invalid: %v", e)
	}
	return p
}

func TestPreviewIsReadOnlyAndClassifiesConflicts(t *testing.T) {
	s := newTestService(t)
	targetPNG, pkgPNG := pngBytes(color.RGBA{1, 2, 3, 255}), pngBytes(color.RGBA{9, 8, 7, 255})

	// Target already has ribbon with different bytes.
	if e := s.Store.Update(func(st *model.State) error {
		st.Assets["ribbon"] = assetRecord("ribbon", targetPNG)
		return nil
	}); e != nil {
		t.Fatal(e)
	}

	pkg := buildPackage(t, imageDoc("ribbon", "extra"), map[string][]byte{
		"ribbon": pkgPNG,
		"extra":  targetPNG,
	})

	prev, e := s.ImportPreview("dest", "导入场景", pkg)
	if e != nil {
		t.Fatalf("preview: %v", e)
	}
	if prev.Fingerprint == "" {
		t.Fatal("preview must carry a fingerprint")
	}
	got := map[string]ImportAssetPreview{}
	for _, a := range prev.Assets {
		got[a.AssetID] = a
	}
	if got["ribbon"].Status != "contentConflict" {
		t.Fatalf("ribbon = %s, want contentConflict", got["ribbon"].Status)
	}
	if !sort.StringsAreSorted([]string{got["ribbon"].Allowed[0], got["ribbon"].Allowed[1]}) &&
		!(contains(got["ribbon"].Allowed, "keep") && contains(got["ribbon"].Allowed, "adopt")) {
		t.Fatalf("ribbon allowed = %v", got["ribbon"].Allowed)
	}
	if got["extra"].Status != "new" {
		t.Fatalf("extra = %s, want new", got["extra"].Status)
	}
	// Read-only: nothing was committed.
	if _, ok := s.Store.Read().Scenes["dest"]; ok {
		t.Fatal("preview must not create the destination scene")
	}
	if s.Store.Read().Assets["ribbon"].Digest != model.Digest(targetPNG) {
		t.Fatal("preview must not touch the target asset")
	}
}

func TestConfirmRejectsStalePreviewWhenAssetChanges(t *testing.T) {
	s := newTestService(t)
	targetPNG, pkgPNG := pngBytes(color.RGBA{1, 2, 3, 255}), pngBytes(color.RGBA{9, 8, 7, 255})
	if e := s.Store.Update(func(st *model.State) error {
		st.Assets["ribbon"] = assetRecord("ribbon", targetPNG)
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	pkg := buildPackage(t, imageDoc("ribbon"), map[string][]byte{"ribbon": pkgPNG})

	prev, e := s.ImportPreview("dest", "导入场景", pkg)
	if e != nil {
		t.Fatal(e)
	}
	// Target asset retires while the user is reviewing the preview.
	if e := s.RetireAsset("ribbon"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ImportConfirm("dest", "导入场景", prev.Fingerprint, pkg,
		map[string]string{"ribbon": "adopt"}); e == nil {
		t.Fatal("expected stale confirm to fail")
	} else {
		var de *model.Error
		if !errors.As(e, &de) || de.Status != 409 {
			t.Fatalf("want 409 conflict, got %v", e)
		}
	}
	// Target stays untouched: no scene, ribbon still retired.
	st := s.Store.Read()
	if _, ok := st.Scenes["dest"]; ok {
		t.Fatal("stale confirm must not create the scene")
	}
	if st.Assets["ribbon"].State != "retired" {
		t.Fatal("retired asset must remain retired")
	}

	// A fresh preview now forces adopt and commits cleanly.
	fresh, e := s.ImportPreview("dest", "导入场景", pkg)
	if e != nil {
		t.Fatal(e)
	}
	row := fresh.Assets[0]
	if row.Status != "retiredConflict" || !contains(row.Allowed, "adopt") || contains(row.Allowed, "keep") {
		t.Fatalf("retired row = %+v", row)
	}
	res, e := s.ImportConfirm("dest", "导入场景", fresh.Fingerprint, pkg,
		map[string]string{"ribbon": "adopt"})
	if e != nil {
		t.Fatalf("refreshed confirm: %v", e)
	}
	newID := res.RemappedAssets["ribbon"]
	if newID == "" || newID == "ribbon" {
		t.Fatalf("adopt must allocate a fresh id, got %q", newID)
	}
	if s.Store.Read().Assets["ribbon"].State != "retired" {
		t.Fatal("retired target id must not be renamed or revived")
	}
	if res.Scene.Document.Layers[0].AssetID != newID {
		t.Fatal("imported document must reference the fresh adopted id")
	}
}

func TestConfirmRejectsStalePreviewWhenSceneAppears(t *testing.T) {
	s := newTestService(t)
	pkgPNG := pngBytes(color.RGBA{9, 8, 7, 255})
	pkg := buildPackage(t, imageDoc("ribbon"), map[string][]byte{"ribbon": pkgPNG})

	prev, e := s.ImportPreview("dest", "导入场景", pkg)
	if e != nil {
		t.Fatal(e)
	}
	// Another writer creates the destination scene during review.
	competing := buildPackage(t, imageDoc("ribbon"), map[string][]byte{"ribbon": pkgPNG})
	if _, e := s.ImportConfirm("holder", "占位", mustFingerprint(t, s, "holder", competing), competing,
		map[string]string{}); e != nil {
		t.Fatal(e)
	}
	// Move holder into the dest id the preview was computed against.
	if e := s.Store.Update(func(st *model.State) error {
		h := st.Scenes["holder"]
		h.ID = "dest"
		st.Scenes["dest"] = h
		delete(st.Scenes, "holder")
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ImportConfirm("dest", "导入场景", prev.Fingerprint, pkg,
		map[string]string{}); e == nil {
		t.Fatal("expected stale confirm after destination scene appeared")
	}
}

func TestKeepPreservesTargetAndAdoptRenamesPackageBytes(t *testing.T) {
	s := newTestService(t)
	targetPNG, pkgPNG := pngBytes(color.RGBA{1, 2, 3, 255}), pngBytes(color.RGBA{9, 8, 7, 255})
	if e := s.Store.Update(func(st *model.State) error {
		st.Assets["ribbon"] = assetRecord("ribbon", targetPNG)
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	pkg := buildPackage(t, imageDoc("ribbon"), map[string][]byte{"ribbon": pkgPNG})

	// keep: target asset untouched, document references the original id.
	prev, _ := s.ImportPreview("kept-scene", "保留", pkg)
	kept, e := s.ImportConfirm("kept-scene", "保留", prev.Fingerprint, pkg,
		map[string]string{"ribbon": "keep"})
	if e != nil {
		t.Fatalf("keep confirm: %v", e)
	}
	if kept.Scene.Document.Layers[0].AssetID != "ribbon" {
		t.Fatal("keep must reference the target id")
	}
	st := s.Store.Read()
	if st.Assets["ribbon"].Digest != model.Digest(targetPNG) {
		t.Fatal("keep must not change target bytes")
	}
	if len(kept.AddedAssets) != 0 || len(kept.ReusedAssets) != 1 {
		t.Fatalf("keep added=%v reused=%v", kept.AddedAssets, kept.ReusedAssets)
	}

	// adopt on a second destination: package bytes under a new id, target intact.
	prev2, _ := s.ImportPreview("adopted-scene", "采用", pkg)
	adopted, e := s.ImportConfirm("adopted-scene", "采用", prev2.Fingerprint, pkg,
		map[string]string{"ribbon": "adopt"})
	if e != nil {
		t.Fatalf("adopt confirm: %v", e)
	}
	newID := adopted.RemappedAssets["ribbon"]
	st = s.Store.Read()
	if st.Assets["ribbon"].Digest != model.Digest(targetPNG) {
		t.Fatal("adopt must leave the target asset bytes untouched")
	}
	if st.Assets[newID].Digest != model.Digest(pkgPNG) {
		t.Fatal("adopt must store package bytes under the new id")
	}
	if adopted.Scene.Document.Layers[0].AssetID != newID {
		t.Fatal("adopt must rewrite the document reference")
	}
}

func TestMissingOrInvalidDecisionFailsAtomically(t *testing.T) {
	s := newTestService(t)
	targetPNG, pkgPNG := pngBytes(color.RGBA{1, 2, 3, 255}), pngBytes(color.RGBA{9, 8, 7, 255})
	if e := s.Store.Update(func(st *model.State) error {
		st.Assets["ribbon"] = assetRecord("ribbon", targetPNG)
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	pkg := buildPackage(t, imageDoc("ribbon"), map[string][]byte{"ribbon": pkgPNG})
	prev, _ := s.ImportPreview("dest", "场景", pkg)

	for _, decisions := range []map[string]string{
		{},
		{"ribbon": "replace"},
		{"unknown": "adopt"},
	} {
		if _, e := s.ImportConfirm("dest", "场景", prev.Fingerprint, pkg, decisions); e == nil {
			t.Fatalf("decisions %v must fail", decisions)
		}
	}
	st := s.Store.Read()
	if len(st.Scenes) != 0 || len(st.Assets) != 1 {
		t.Fatalf("failed confirms must not land anything: scenes=%d assets=%d", len(st.Scenes), len(st.Assets))
	}
}

func TestOneShotImportConflictReturnsPreview(t *testing.T) {
	s := newTestService(t)
	targetPNG, pkgPNG := pngBytes(color.RGBA{1, 2, 3, 255}), pngBytes(color.RGBA{9, 8, 7, 255})
	if e := s.Store.Update(func(st *model.State) error {
		st.Assets["ribbon"] = assetRecord("ribbon", targetPNG)
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	pkg := buildPackage(t, imageDoc("ribbon"), map[string][]byte{"ribbon": pkgPNG})

	if _, e := s.Import("dest", "场景", pkg); e == nil {
		t.Fatal("conflicting one-shot import must fail")
	} else {
		var pe *PreviewConflictError
		if !errors.As(e, &pe) {
			t.Fatalf("want PreviewConflictError, got %T %v", e, e)
		}
		if pe.Preview.Fingerprint == "" || pe.Preview.Assets[0].Status != "contentConflict" {
			t.Fatalf("conflict must carry an actionable preview: %+v", pe.Preview)
		}
	}
	// No-conflict one-shot still commits directly.
	clean := buildPackage(t, imageDoc("fresh"), map[string][]byte{"fresh": pkgPNG})
	scene, e := s.Import("clean-dest", "干净", clean)
	if e != nil {
		t.Fatalf("clean import: %v", e)
	}
	if scene.ID != "clean-dest" {
		t.Fatalf("unexpected scene %+v", scene)
	}
}

func mustFingerprint(t *testing.T, s *Service, id string, p bundle.Package) string {
	t.Helper()
	prev, e := s.ImportPreview(id, "占位", p)
	if e != nil {
		t.Fatal(e)
	}
	return prev.Fingerprint
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
