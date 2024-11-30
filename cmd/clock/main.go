package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/flosch/pongo2/v6"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	r chi.Router
	n *http.Server

	t time.Time

	tmpls *pongo2.TemplateSet
}

func main() {
	os.Exit(func() int {
		sbl, err := pongo2.NewSandboxedFilesystemLoader("theme/p2")
		if err != nil {
			slog.Error("Error loading templates", "error", err)
			return 2
		}

		s := Server{
			r:     chi.NewRouter(),
			n:     &http.Server{},
			tmpls: pongo2.NewSet("html", sbl),
		}
		s.tmpls.Debug = true

		s.r.Use(middleware.Heartbeat("/healthz"))
		s.r.Get("/admin", s.admin)
		s.r.Get("/clock", s.clock)
		s.r.Get("/clock/run", s.clockRun)
		s.r.Post("/clock/start", s.clockStart)
		s.r.Post("/clock/cancel", s.clockCancel)
		s.r.Get("/clock/end", s.clockEnd)
		s.fileServer(s.r, "/static", http.Dir("theme/static"))

		serverCtx, serverStopCtx := context.WithCancel(context.Background())
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
		go func() {
			<-sig

			shutdownCtx, _ := context.WithTimeout(serverCtx, 30*time.Second)

			go func() {
				<-shutdownCtx.Done()
				if shutdownCtx.Err() == context.DeadlineExceeded {
					slog.Error("Graceful shutdown timed out.. forcing exit.")
					os.Exit(5)
				}
			}()

			err := s.Shutdown(shutdownCtx)
			if err != nil {
				slog.Error("Error occured during shutdown", "error", err)
			}
			serverStopCtx()

		}()

		bind := os.Getenv("CLOCK_ADDR")
		if bind == "" {
			bind = ":1323"
		}
		s.Serve(bind)
		<-serverCtx.Done()
		return 0
	}())
}

// Serve binds, initializes the mux, and serves forever.
func (s *Server) Serve(bind string) error {
	slog.Info("HTTP is starting")
	s.n.Addr = bind
	s.n.Handler = s.r
	return s.n.ListenAndServe()
}

// Shutdown requests the underlying server gracefully cease operation.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.n.Shutdown(ctx)
}

func (s *Server) templateErrorHandler(w http.ResponseWriter, err error) {
	fmt.Fprintf(w, "Error while rendering template: %s\n", err)
}

func (s *Server) fileServer(r chi.Router, path string, root http.FileSystem) {
	if strings.ContainsAny(path, "{}*") {
		panic("FileServer does not permit any URL parameters.")
	}

	if path != "/" && path[len(path)-1] != '/' {
		r.Get(path, http.RedirectHandler(path+"/", http.StatusMovedPermanently).ServeHTTP)
		path += "/"
	}
	path += "*"

	r.Get(path, func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.RouteContext(r.Context())
		pathPrefix := strings.TrimSuffix(rctx.RoutePattern(), "/*")
		fs := http.StripPrefix(pathPrefix, http.FileServer(root))
		fs.ServeHTTP(w, r)
	})
}

func (s *Server) doTemplate(w http.ResponseWriter, r *http.Request, tmpl string, ctx pongo2.Context) {
	if ctx == nil {
		ctx = pongo2.Context{}
	}
	t, err := s.tmpls.FromCache(tmpl)
	if err != nil {
		s.templateErrorHandler(w, err)
		return
	}
	if err := t.ExecuteWriter(ctx, w); err != nil {
		s.templateErrorHandler(w, err)
	}
}

func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	s.doTemplate(w, r, "views/admin.p2", pongo2.Context{})
}

func (s *Server) clock(w http.ResponseWriter, r *http.Request) {
	s.doTemplate(w, r, "views/clock.p2", pongo2.Context{})
}

func (s *Server) clockEnd(w http.ResponseWriter, r *http.Request) {
	d := struct {
		MatchEnd time.Time
	}{
		MatchEnd: s.t,
	}

	json.NewEncoder(w).Encode(d)
}

func (s *Server) clockStart(w http.ResponseWriter, r *http.Request) {
	if s.t.After(time.Now()) {
		w.WriteHeader(http.StatusPreconditionFailed)
		w.Write([]byte("clock already running!\n"))
		return
	}

	s.t = time.Now().Add(time.Minute*3 + time.Second)
}

func (s *Server) clockCancel(w http.ResponseWriter, r *http.Request) {
	s.t = time.Now()
}

func (s *Server) clockRun(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(s.t.After(time.Now()))
}
