package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/moonrhythm/parapet"
	"github.com/moonrhythm/parapet/pkg/location"
	"github.com/moonrhythm/parapet/pkg/logger"
	"github.com/moonrhythm/parapet/pkg/prom"
	"github.com/moonrhythm/parapet/pkg/stripprefix"
	"github.com/moonrhythm/parapet/pkg/upstream"
)

var (
	ethClient       *ethclient.Client
	blockDuration   time.Duration
	healthyDuration time.Duration
)

func main() {
	var (
		addr                = flag.String("addr", ":80", "address to listening")
		gethAddr            = flag.String("geth.addr", "127.0.0.1", "geth address")
		gethHTTP            = flag.String("geth.http", "8545", "geth http port")
		gethWS              = flag.String("geth.ws", "8546", "geth ws port")
		gethMetrics         = flag.String("geth.metrics", "6060", "geth metrics port")
		gethBlockUnit       = flag.Duration("geth.block-unit", time.Second, "block timestamp unit")
		gethHealthzDuration = flag.Duration("geth.healthy-duration", time.Minute, "duration from last block that mark as healthy")
	)

	flag.Parse()

	// TODO: lazy dial ?
	var err error
	ethClient, err = ethclient.Dial("http://" + *gethAddr + ":" + *gethHTTP)
	if err != nil {
		log.Fatalf("can not dial geth; %v", err)
	}
	blockDuration = *gethBlockUnit
	healthyDuration = *gethHealthzDuration

	srv := parapet.NewBackend()
	srv.Addr = *addr
	srv.GraceTimeout = 3 * time.Second
	srv.WaitBeforeShutdown = 0

	srv.Use(logger.Stdout())
	srv.Use(prom.Requests())

	// healthz
	{
		l := location.Exact("/healthz")
		l.Use(parapet.Handler(healthz))
		srv.Use(l)
	}

	// websocket
	if *gethWS != "" {
		l := location.Exact("/ws")
		l.Use(stripprefix.New("/ws"))
		l.Use(upstream.SingleHost(*gethAddr+":"+*gethWS, &upstream.HTTPTransport{}))
		srv.Use(l)
	}

	// metrics
	if *gethMetrics != "" {
		l := location.Prefix("/metrics/")

		// /geth
		{
			p := location.Exact("/metrics/geth")
			p.Use(rewritePath("/debug/metrics/prometheus"))
			p.Use(upstream.SingleHost(*gethAddr+":"+*gethMetrics, &upstream.HTTPTransport{}))
			l.Use(p)
		}

		// /proxy
		{
			p := location.Exact("/metrics/proxy")
			p.Use(wrapHandler(prom.Handler()))
			l.Use(p)
		}

		l.Use(wrapHandler(http.NotFoundHandler()))

		srv.Use(l)
	}

	// http
	srv.Use(upstream.SingleHost(*gethAddr+":"+*gethHTTP, &upstream.HTTPTransport{
		MaxIdleConns: 10000,
	}))

	prom.Connections(srv)
	prom.Networks(srv)

	err = srv.ListenAndServe()
	if err != nil {
		log.Fatalf("can not start server; %v", err)
	}
}

var lastBlock struct {
	mu        sync.Mutex
	Block     *types.Block
	UpdatedAt time.Time
}

func getLastBlock(ctx context.Context) (*types.Block, error) {
	lastBlock.mu.Lock()
	defer lastBlock.mu.Unlock()

	if time.Since(lastBlock.UpdatedAt) < time.Second {
		return lastBlock.Block, nil
	}

	block, err := ethClient.BlockByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	lastBlock.Block = block
	lastBlock.UpdatedAt = time.Now()
	return lastBlock.Block, nil
}

func isReady(ctx context.Context) (bool, error) {
	block, err := getLastBlock(ctx)
	if err != nil {
		return false, err
	}
	ts := block.Time()
	ts = ts * uint64(blockDuration) // convert to ns
	t := time.Unix(0, int64(ts))
	return time.Since(t) < healthyDuration, nil
}

func isLive(ctx context.Context) bool {
	_, err := getLastBlock(ctx)
	return err == nil
}

func healthz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.FormValue("ready") == "1" {
		ready, err := isReady(ctx)
		if err != nil {
			http.Error(w, "can not get block", http.StatusInternalServerError)
			return
		}
		if !ready {
			http.Error(w, "not ready", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
		return
	}

	live := isLive(ctx)
	if !live {
		http.Error(w, "not ok", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func rewritePath(path string) parapet.Middleware {
	return parapet.MiddlewareFunc(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.URL.Path = path
			h.ServeHTTP(w, r)
		})
	})
}

func wrapHandler(h http.Handler) parapet.Middleware {
	return parapet.MiddlewareFunc(func(http.Handler) http.Handler {
		return h
	})
}
