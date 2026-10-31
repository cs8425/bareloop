package server

import (
	"net/http"
	"net/http/pprof"
)

// PprofHandler is Go's runtime profiler: the five endpoints net/http/pprof registers on
// DefaultServeMux. The daemon either serves them on a listener of its own (when -pprof names an
// address) or mounts them on the API routes (EnablePprof).
//
// None of this is authenticated, and /debug/pprof/profile answers by holding the connection for
// thirty seconds of CPU sampling — which is why it is off unless the operator asks for it, and why the
// startup line names whichever port is exposing it. A cross-origin page cannot read any of these
// answers, but a page can still make the browser ask for a profile, so "same" on a non-loopback -addr
// costs the warning main logs.
func PprofHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// EnablePprof mounts those routes on the API handler, and has to be called before Handler(). The
// routes beat the catch-all "/" because ServeMux prefers the more specific pattern, so the UI keeps
// being served and /debug/pprof/ stops being looked up as a file.
func (s *Server) EnablePprof() { s.pprof = true }
