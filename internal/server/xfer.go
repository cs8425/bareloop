package server

// The import/export pair. Both are deliberately API-streaming: the server never writes a
// transcript to its own disk (a second lifecycle for exports — cleanup, name collisions,
// which copy is current — is a machine nobody asked to build; curl-to-file is the same
// stream), and an import arrives as the bytes the caller holds, not as a path the server
// would have to be careful with. The Content-Encoding header stays identity on exports by
// the same honesty rule the filename carries: the bytes named .zst are the bytes the
// client stored, whatever proxy sits in between.

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"bareloop/internal/agent"
	"bareloop/internal/ir"
	"bareloop/internal/xfer"
)

// handleExport streams GET /api/session/{id}/export: line 1 the header (config without
// secrets), then every entry as the transcript endpoint itself would answer it. No page
// ceiling stands in — this is the controlled dump, not the list API — and the whole thing
// rides out as one stream.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	st, err := sess.Log().Replay()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	hdr := xfer.Header{Session: xfer.Session{ID: sess.ID, Name: sess.Config().Name}}
	var entries []*ir.Entry
	if p := strings.TrimSpace(q.Get("path")); p != "" {
		at := p
		switch strings.ToLower(at) {
		case "tip", "head", "active", "path":
			at = st.Tip
		}
		entries = ir.Path(st.Entries, at)
		if len(entries) == 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("entry %q is not on this session's tree", at))
			return
		}
		hdr.Tip = entries[len(entries)-1].ID
		hdr.Scope = xfer.ScopePath
	} else {
		entries = ir.TreeOrder(st.Entries)
		hdr.Tip = st.Tip
		hdr.Scope = xfer.ScopeFull
	}
	if len(entries) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("this session has nothing to export"))
		return
	}
	codec := strings.ToLower(strings.TrimSpace(q.Get("codec")))
	if codec == "" {
		codec = xfer.CodecZstd
	}
	payload, closer, err := xfer.CompressWriter(codec, w)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if closer != nil {
		defer closer.Close()
	}
	hdr.Session.Config = xfer.ScrubbedConfig(sess.Config())
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="`+exportName(sess, codec)+`"`)
	if err := xfer.Encode(payload, hdr, entries); err != nil {
		// Headers are long gone; a stream that dies mid-file is a stream the client
		// sees die, which is the only honest report left at this point.
		s.logf("export %s: stream broke: %v", sess.ID, err)
	}
}

// exportName follows the webui's file-naming habit (minute-precision time, a conv tag, a
// slug of the name) so a folder of exports sorts like a folder of exports whatever tool
// made them. The codec earns its extension; nothing lies about what it is.
func exportName(sess *agent.Session, codec string) string {
	info, err := sess.Log().Read()
	last := time.Now().UTC()
	if err == nil && !info.LastTs.IsZero() {
		last = info.LastTs.UTC()
	}
	base := sess.Config().Name
	if base == "" {
		base = sess.ID
	}
	id := sess.ID
	if len(id) > 10 {
		id = id[:10]
	}
	ext := ".jsonl"
	switch codec {
	case xfer.CodecGzip, "gz":
		ext += ".gz"
	case xfer.CodecZstd, "zst":
		ext += ".zst"
	}
	return last.Format("2006-01-02_15-04") + "_conv_" + id + "_" + slug(base) + ext
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugRe.ReplaceAllString(s, "_")
	if len(s) > 40 {
		s = strings.TrimSuffix(s[:40], "_")
	}
	if s == "" {
		s = "session"
	}
	return s
}

// handleImport is POST /api/sessions/import?id=…[&name=…]: the body is a transcript file,
// compressed or not, native or webui — the magic bytes and the first line decide, not a
// query parameter or a file name. The new session's id is required and the caller's: an
// import lands next to its sources, never on top of one.
//
// Two doors stand on the body: MaxCompressedBody caps what a request may carry,
// MaxDecompressed caps what it may unfold into, each enforced as its stream is read — a
// decompression bomb never exists in full anywhere, least of all in memory. What survives
// lands in one store transaction: a file that fails on line 400 creates no session at
// all, because half an import is the broken tree the reader refused to write.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, errors.New("an id is required to import (?id=…)"))
		return
	}
	if err := agent.ValidID(id); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if r.ContentLength > xfer.MaxCompressedBody {
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Errorf("import body is %d bytes over the %d byte limit", r.ContentLength, xfer.MaxCompressedBody))
		return
	}
	importErr := func(err error) {
		var tooBig *http.MaxBytesError
		switch {
		case errors.Is(err, xfer.ErrTooLarge), errors.As(err, &tooBig):
			writeErr(w, http.StatusRequestEntityTooLarge, err)
		default:
			writeErr(w, http.StatusBadRequest, err)
		}
	}
	plain, closer, err := xfer.Uncompress(http.MaxBytesReader(w, r.Body, xfer.MaxCompressedBody))
	if closer != nil {
		defer closer.Close()
	}
	if err != nil {
		importErr(err)
		return
	}
	stream, kind, err := xfer.Sniff(plain)
	if err != nil {
		importErr(err)
		return
	}
	var imp *xfer.Import
	switch kind {
	case xfer.KindNative:
		imp, err = xfer.DecodeNative(stream)
	case xfer.KindWebUI:
		imp, err = xfer.DecodeWebUI(stream)
	}
	if err != nil {
		importErr(err)
		return
	}

	// The config the file carried (native exports only) seeds the session; the name the
	// caller gave the query outranks the name the file remembers. Secrets stripped on the
	// way out stay stripped on the way in — a file that arrives with an API key someone
	// pasted in is not a reason to store it.
	cfg := agent.Config{ID: id}
	if imp.Config != nil {
		cfg = *imp.Config
		cfg.ID = id
		cfg.LLM.APIKey = ""
		cfg.LLM.Headers = nil
	}
	if n := strings.TrimSpace(r.URL.Query().Get("name")); n != "" {
		cfg.Name = n
	} else if cfg.Name == "" && imp.Name != "" {
		cfg.Name = imp.Name
	}
	if imp.System != "" && strings.TrimSpace(cfg.SystemPrompt) == "" {
		cfg.SystemPrompt = imp.System
	}

	dst, err := s.mgr.Create(r.Context(), cfg)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			writeErr(w, http.StatusConflict, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	wrote := false
	defer func() {
		if !wrote {
			if err := s.mgr.Remove(id); err != nil {
				s.logf("import %q: could not undo the failed import: %v", id, err)
			}
		}
	}()
	if err := dst.Log().ImportAll(imp.Entries, imp.Tip); err != nil {
		importErr(err) // a duplicate id inside the file, a corrupt payload: the file's fault, 400
		return
	}
	// The opened session re-reads what it just wrote, so the caller's first GET after
	// the import answers with the imported story, not with the empty moment Create
	// opened.
	if err := dst.Reload(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	wrote = true
	writeJSON(w, map[string]any{"id": id, "format": kind, "entries": len(imp.Entries), "status": dst.Status()})
}
