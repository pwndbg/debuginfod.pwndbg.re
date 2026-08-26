package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"time"

	"github.com/julienschmidt/httprouter"

	"github.com/pwndbg/debuginfod.pwndbg.re/nix"
)

// globalStore is set in main. It is a package-level variable for the same reason
// cmd/nix-nar-old has one: the handler signature has nowhere to hang state.
var globalStore *store

func executableHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
	return globalStore.GetExecutable(r.Context(), ps.ByName("buildid"), w)
}

func sourceHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
	return globalStore.GetSource(r.Context(), ps.ByName("buildid"), ps.ByName("path"), w)
}

func debuginfoHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
	return globalStore.GetDebuginfo(r.Context(), ps.ByName("buildid"), w)
}

func main() {
	if err := ParseConfig(); err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()

	globalStore = newStore(
		nix.NewNixDebuginfo(log),
		Config.ImagePath,
		Config.MountRoot,
		Config.EntryMountPath,
		Config.MaxFetches,
	)

	db, err := NewDB(ctx, Config.ClickhouseDSN)
	if err != nil {
		log.Fatal(err)
	}
	if err := db.Init(ctx); err != nil {
		log.Fatal(err)
	}

	router := httprouter.New()
	router.GET("/buildid/:buildid/executable", AccessLogMiddleware(db, "executable", executableHandler))
	router.GET("/buildid/:buildid/debuginfo", AccessLogMiddleware(db, "debuginfo", debuginfoHandler))
	router.GET("/buildid/:buildid/source/*path", AccessLogMiddleware(db, "source", sourceHandler))

	log.WithField("addr", Config.ListenAddr).Info("nix backend starting")
	srv := &http.Server{
		Handler:      router,
		Addr:         Config.ListenAddr,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Minute, // downloading big files
	}

	if !Config.TLSEnabled {
		log.Warn("TLS disabled; HTTP/2 and therefore Early Hints will not be negotiated")
		if err := srv.ListenAndServe(); err != nil {
			log.Fatalf("Server failed: %v", err)
		}
		return
	}

	cert, err := selfSignedCert(certHosts(Config.ListenAddr))
	if err != nil {
		log.Fatalf("Server failed: %v", err)
	}
	srv.TLSConfig = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	log.WithField("hosts", certHosts(Config.ListenAddr)).
		WithField("expires", cert.Leaf.NotAfter.Format(time.RFC3339)).
		Info("using an in-memory self-signed certificate; callers must skip verification")

	// Empty paths: the certificate is in TLSConfig, not on disk.
	if err := srv.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
