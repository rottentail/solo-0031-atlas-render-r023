package httpapi

import (
	"atlas-render/internal/bundle"
	"net/http"
)

// importPreview returns a read-only, human-understandable conflict preview.
// It never changes the target state.
func (a API) importPreview(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name    string         `json:"name"`
		Package bundle.Package `json:"package"`
	}
	if e := decode(w, r, &b); e != nil {
		fail(w, e)
		return
	}
	prev, e := a.Service.ImportPreview(r.PathValue("id"), b.Name, b.Package)
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, prev)
}

// importCommit applies a previously previewed import after re-validating the
// target inside the write transaction, so stale previews can never be applied.
func (a API) importCommit(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name        string            `json:"name"`
		Fingerprint string            `json:"fingerprint"`
		Package     bundle.Package    `json:"package"`
		Decisions   map[string]string `json:"decisions"`
	}
	if e := decode(w, r, &b); e != nil {
		fail(w, e)
		return
	}
	res, e := a.Service.ImportConfirm(r.PathValue("id"), b.Name, b.Fingerprint, b.Package, b.Decisions)
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 201, res)
}

// importPackage is the legacy one-shot entry point. A conflict now returns the
// preview body alongside the 409 so clients can resolve and confirm explicitly.
func (a API) importPackage(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name    string         `json:"name"`
		Package bundle.Package `json:"package"`
	}
	if e := decode(w, r, &b); e != nil {
		fail(w, e)
		return
	}
	v, e := a.Service.Import(r.PathValue("id"), b.Name, b.Package)
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 201, v)
}
