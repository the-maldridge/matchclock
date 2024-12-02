package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/faiface/beep"
	"github.com/faiface/beep/speaker"
	"github.com/faiface/beep/wav"
	"github.com/flosch/pongo2/v6"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Server binds all the components of the webserver.
type Server struct {
	r chi.Router
	n *http.Server

	t  time.Time
	et *time.Timer
	wt *time.Timer

	tmpls *pongo2.TemplateSet
}

//go:embed theme
var efs embed.FS

// IPAccessList checks if an IP is contained within MATCHCLOCK_ACL
func IPAccessList(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := net.ParseIP(strings.Split(r.RemoteAddr, ":")[0])

		block := os.Getenv("MATCHCLOCK_ACL")
		if block == "" {
			block = "0.0.0.0/0"
		}

		_, acl, _ := net.ParseCIDR(block)
		if !acl.Contains(ip) {
			slog.Warn("Unauthorized access", "ip", ip)
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("Go Away!\n"))
			return
		}

		next.ServeHTTP(w, r)
	})
}

func main() {
	os.Exit(func() int {
		var tfs fs.FS
		tfs, _ = fs.Sub(efs, "theme")
		if _, ok := os.LookupEnv("MATCHCLOCK_DEBUG"); ok {
			slog.Info("Debug mode enabled")
			tfs = os.DirFS("theme")
		}
		tsfs, _ := fs.Sub(tfs, "p2")
		s := Server{
			r:     chi.NewRouter(),
			n:     &http.Server{},
			tmpls: pongo2.NewSet("html", pongo2.NewFSLoader(tsfs)),
		}

		if _, ok := os.LookupEnv("MATCHCLOCK_DEBUG"); ok {
			s.tmpls.Debug = true
		}

		s.r.Use(middleware.Heartbeat("/healthz"))
		s.r.Route("/admin", func(r chi.Router) {
			r.Use(IPAccessList)
			r.Route("/clock", func(cr chi.Router) {
				cr.Get("/", s.clockAdmin)
				cr.Post("/start", s.clockStart)
				cr.Post("/cancel", s.clockCancel)
			})
			r.Route("/sound", func(sr chi.Router) {
				sr.Get("/", s.soundAdmin)
				sr.Post("/play/{file}", s.soundAdminPlay)
			})
		})

		s.r.Route("/clock", func(r chi.Router) {
			r.Get("/", s.clock)
			r.Get("/run", s.clockRun)
			r.Get("/end", s.clockEnd)
		})

		sfs, _ := fs.Sub(tfs, "static")
		s.fileServer(s.r, "/static", http.FS(sfs))

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

func (s *Server) clockAdmin(w http.ResponseWriter, r *http.Request) {
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

	s.t = time.Now().Add(time.Minute*3 + time.Millisecond*500)
	s.playSound("start-of-match.wav")
	s.et = time.AfterFunc(s.t.Sub(time.Now()), func() { s.playSound("end-of-match.wav") })
	s.wt = time.AfterFunc(s.t.Sub(time.Now())-time.Second*30, func() { s.playSound("match-almost-over.wav") })
}

func (s *Server) clockCancel(w http.ResponseWriter, r *http.Request) {
	s.t = time.Now()
	s.et.Stop()
	s.wt.Stop()
	s.playSound("match-fault.wav")
}

func (s *Server) clockRun(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(s.t.After(time.Now()))
}

func (s *Server) soundAdmin(w http.ResponseWriter, r *http.Request) {
	s.doTemplate(w, r, "views/soundboard.p2", pongo2.Context{})
}

func (s *Server) soundAdminPlay(w http.ResponseWriter, r *http.Request) {
	file := chi.URLParam(r, "file")
	s.playSound(file)
}

func (s *Server) playSound(file string) {
	f, err := efs.Open(filepath.Join("theme", "sound", file))
	if err != nil {
		slog.Error("Failure to open sound", "file", file, "error", err)
		return
	}
	defer f.Close()
	snd, format, err := wav.Decode(f)
	if err != nil {
		fmt.Println(err)
		return
	}
	speaker.Init(format.SampleRate, format.SampleRate.N(time.Second/10))
	done := make(chan struct{})
	speaker.Play(beep.Seq(snd, beep.Callback(func() {
		close(done)
	})))
	<-done
	speaker.Close()
}
