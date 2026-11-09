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
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"bareloop/internal/agent"
	"bareloop/internal/ir"
	"bareloop/internal/store"
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
	hdr := xfer.Header{Session: xfer.Session{ID: sess.ID, Name: sess.Config().Name}}
	// The three shapes differ only in which ids they stream and in what the header
	// says about them. A resident session answers "which ids, in what order" from
	// its spine — the tree is asked its shape without a single payload read for it
	// — and every row then travels through one page-sized cursor: an export of a
	// fifty-megabyte session costs the same RAM as one of fifty rows.
	var ids []string
	var rw func(int, *ir.Entry) *ir.Entry
	tip := ""
	if info, err := sess.Log().Read(); err == nil {
		tip = info.Tip
	}
	switch p := strings.TrimSpace(q.Get("path")); strings.ToLower(p) {
	case "ctx":
		// The window the next request carries, by the one rule the ctx fork will
		// later pay from the same code (ir.WindowRewriter).
		var brokenCmp string
		ids, brokenCmp = sess.WindowIDs()
		rw = ir.WindowRewriter(ids, brokenCmp)
		hdr.Tip = tip
		hdr.Scope = xfer.ScopeCtx
	case "":
		ids = sess.TreeIDs()
		hdr.Tip = tip
		hdr.Scope = xfer.ScopeFull
	default:
		at := p
		switch strings.ToLower(at) {
		case "tip", "head", "active", "path":
			at = tip
		}
		ids = sess.PathIDs(at)
		if len(ids) == 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("entry %q is not on this session's tree", at))
			return
		}
		hdr.Tip = ids[len(ids)-1]
		hdr.Scope = xfer.ScopePath
	}
	if len(ids) == 0 {
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
	if err := xfer.EncodeStream(payload, hdr, rowCursor(sess.Log(), ids, rw)); err != nil {
		// Headers are long gone; a stream that dies mid-file is a stream the client
		// sees die, which is the only honest report left at this point.
		s.logf("export %s: stream broke: %v", sess.ID, err)
	}
}

// rowCursor reads the named ids through the store a page at a time: each page is
// read, written, and forgotten before the next is asked for, and the export itself
// holds nothing larger than the id list. A row that is not there when its page
// arrives (deleted mid-stream) stops the stream — a silently missing line would be
// a file whose parent walks no importer will take, and a dead stream logs.
func rowCursor(log *store.Log, ids []string, rw func(int, *ir.Entry) *ir.Entry) func() (*ir.Entry, error) {
	const pageSize = 100
	pos, at, out := 0, 0, 0
	var order []string
	var page map[string]*ir.Entry
	return func() (*ir.Entry, error) {
		for at >= len(order) {
			if pos >= len(ids) {
				return nil, io.EOF
			}
			end := pos + pageSize
			if end > len(ids) {
				end = len(ids)
			}
			order = ids[pos:end]
			pos = end
			at = 0
			var err error
			page, err = log.Contents(order, true)
			if err != nil {
				return nil, err
			}
		}
		id := order[at]
		at++
		e, ok := page[id]
		if !ok {
			return nil, fmt.Errorf("entry %s is not in the store while the export read it", id)
		}
		if rw != nil {
			e = rw(out, e)
		}
		out++
		return e, nil
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
		case errors.Is(err, agent.ErrMCPLocked):
			// A bundle that carries MCP servers this daemon will not host is the lock
			// speaking, not a bad file: import is how a session — or anything that can
			// POST — could otherwise walk a server set past mcpLock.
			writeErr(w, http.StatusForbidden, err)
		case strings.Contains(err.Error(), "already exists"):
			writeErr(w, http.StatusConflict, err)
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

	// The config the file carried (the native dialect carries one, the webui dialect
	// offers at most a system prompt) seeds the session; the name the caller gave the
	// query outranks the name the file remembers. Secrets stripped on the way out stay
	// stripped on the way in — a file that arrives with an API key someone pasted in is
	// not a reason to store it.
	cfgFor := func(fileCfg *agent.Config, fileName, system string) agent.Config {
		cfg := agent.Config{ID: id}
		if fileCfg != nil {
			cfg = *fileCfg
			cfg.ID = id
			cfg.LLM.APIKey = ""
			cfg.LLM.Headers = nil
		}
		if n := strings.TrimSpace(r.URL.Query().Get("name")); n != "" {
			cfg.Name = n
		}
		if cfg.Name == "" && fileName != "" {
			cfg.Name = fileName
		}
		if system != "" && strings.TrimSpace(cfg.SystemPrompt) == "" {
			cfg.SystemPrompt = system
		}
		return cfg
	}

	// An import is all rows or none. The native route streams: rows land as the
	// decoder reads them, the two whole-file verdicts (the tip among the rows, every
	// parent a row that is there) are Finish's SELECTs over what was written, and a
	// failed attempt is unwound twice over — the living transaction unwinds the rows,
	// this route deletes the directory. The webui dialect stays whole: its
	// parent-bypass walk wants the file in hand, and it is the legacy shape that
	// carries no megabytes of images — refactoring it would be paying to stream the
	// dialect nobody imports while the one everybody imports already streams.
	var (
		dst                    *agent.Session
		itx                    *store.ImportTx
		created, wrote, txOpen bool
	)
	defer func() {
		if txOpen {
			if err := itx.Abort(); err != nil {
				s.logf("import %q: could not unwind the failed transaction: %v", id, err)
			}
		}
		if created && !wrote {
			if err := s.mgr.Remove(id); err != nil {
				s.logf("import %q: could not undo the failed import: %v", id, err)
			}
		}
	}()

	var entries int
	if kind == xfer.KindNative {
		tip := ""
		err := xfer.DecodeNativeStream(stream, func(h *xfer.Header) error {
			sess, cerr := s.mgr.Create(r.Context(), cfgFor(h.Session.Config, h.Session.Name, ""))
			if cerr != nil {
				return cerr
			}
			dst, created = sess, true
			tx, terr := dst.Log().ImportStream()
			if terr != nil {
				return terr
			}
			itx, txOpen, tip = tx, true, h.Tip
			return nil
		}, func(e *ir.Entry) error {
			werr := itx.Entry(e)
			if werr != nil && strings.Contains(werr.Error(), "UNIQUE constraint") {
				return fmt.Errorf("duplicate entry id %q", e.ID) // the key's own sentence in the file's words
			}
			return werr
		})
		if err != nil {
			importErr(err)
			return
		}
		if err := itx.Finish(tip); err != nil {
			txOpen = false // Finish consumed the transaction, committed or not
			importErr(err)
			return
		}
		txOpen = false
		entries = itx.Rows()
	} else {
		imp, derr := xfer.DecodeWebUI(stream)
		if derr != nil {
			importErr(derr)
			return
		}
		sess, cerr := s.mgr.Create(r.Context(), cfgFor(imp.Config, imp.Name, imp.System))
		if cerr != nil {
			importErr(cerr)
			return
		}
		dst, created = sess, true
		if err := dst.Log().ImportAll(imp.Entries, imp.Tip); err != nil {
			importErr(err) // a duplicate id inside the file, a corrupt payload: the file's fault, 400
			return
		}
		entries = len(imp.Entries)
	}
	// The opened session re-reads what it just wrote, so the caller's first GET after
	// the import answers with the imported story, not with the empty moment Create
	// opened.
	if err := dst.Reload(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	wrote = true
	writeJSON(w, map[string]any{"id": id, "format": kind, "entries": entries, "status": dst.Status()})
}
