package main

import (
	"context"
	"crypto/tls"
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
	"github.com/prometheus/client_golang/prometheus"
)

var (
	ethClient       *ethclient.Client
	blockDuration   time.Duration
	healthyDuration time.Duration
)

func main() {
	var (
		addr                = flag.String("addr", ":80", "http address")
		tlsAddr             = flag.String("tls.addr", ":443", "tls address")
		tlsKey              = flag.String("tls.key", "", "TLS private key file")
		tlsCert             = flag.String("tls.cert", "", "TLS certificate file")
		logEnable           = flag.Bool("log", true, "Enable request log")
		gethAddr            = flag.String("geth.addr", "127.0.0.1", "geth address")
		gethHTTP            = flag.String("geth.http", "8545", "geth http port")
		gethWS              = flag.String("geth.ws", "8546", "geth ws port")
		gethMetrics         = flag.String("geth.metrics", "6060", "geth metrics port")
		gethBlockUnit       = flag.Duration("geth.block-unit", time.Second, "block timestamp unit")
		gethHealthyDuration = flag.Duration("geth.healthy-duration", time.Minute, "duration from last block that mark as healthy")
	)

	flag.Parse()

	log.Printf("geth-proxy")
	log.Printf("HTTP address: %s", *addr)
	log.Printf("HTTPS address: %s", *tlsAddr)
	log.Printf("Geth address: %s", *gethAddr)
	log.Printf("Geth http Port: %s", *gethHTTP)
	log.Printf("Geth ws Port: %s", *gethWS)
	log.Printf("Geth metrics port: %s", *gethMetrics)
	log.Printf("Geth block unit: %s", *gethBlockUnit)
	log.Printf("Geth healthy-duration: %s", *gethHealthyDuration)

	// TODO: lazy dial ?
	var err error
	ethClient, err = ethclient.Dial("http://" + *gethAddr + ":" + *gethHTTP)
	if err != nil {
		log.Fatalf("can not dial geth; %v", err)
	}
	blockDuration = *gethBlockUnit
	healthyDuration = *gethHealthyDuration

	prom.Registry().MustRegister(headDuration)
	go prom.Start(":6060")
	go func() {
		// update stats

		for {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			promUpdateHeadDuration(ctx)
			cancel()

			time.Sleep(time.Second)
		}
	}()

	var s parapet.Middlewares

	if *logEnable {
		s.Use(logger.Stdout())
	}
	s.Use(prom.Requests())

	// healthz
	{
		l := location.Exact("/healthz")
		l.Use(parapet.Handler(healthz))
		s.Use(l)
	}

	// websocket
	if *gethWS != "" {
		l := location.Exact("/ws")
		l.Use(stripprefix.New("/ws"))
		l.Use(upstream.SingleHost(*gethAddr+":"+*gethWS, &upstream.HTTPTransport{}))
		s.Use(l)
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

		s.Use(l)
	}

	// http
	s.Use(upstream.SingleHost(*gethAddr+":"+*gethHTTP, &upstream.HTTPTransport{
		MaxIdleConns: 10000,
	}))

	var wg sync.WaitGroup

	if *addr != "" {
		wg.Add(1)
		srv := parapet.NewBackend()
		srv.Addr = *addr
		srv.GraceTimeout = 3 * time.Second
		srv.WaitBeforeShutdown = 0
		srv.Use(s)
		prom.Connections(srv)
		prom.Networks(srv)
		go func() {
			defer wg.Done()

			err = srv.ListenAndServe()
			if err != nil {
				log.Fatalf("can not start server; %v", err)
			}
		}()
	}

	if *tlsAddr != "" {
		wg.Add(1)
		srv := parapet.NewBackend()
		srv.Addr = *tlsAddr
		srv.GraceTimeout = 3 * time.Second
		srv.WaitBeforeShutdown = 0
		srv.TLSConfig = &tls.Config{}

		if *tlsKey == "" || *tlsCert == "" {
			cert, err := parapet.GenerateSelfSignCertificate(parapet.SelfSign{
				CommonName: "geth-proxy",
				Hosts:      []string{"geth-proxy"},
				NotBefore:  time.Now().Add(-5 * time.Minute),
				NotAfter:   time.Now().AddDate(10, 0, 0),
			})
			if err != nil {
				log.Fatalf("can not generate self signed cert; %v", err)
			}
			srv.TLSConfig.Certificates = append(srv.TLSConfig.Certificates, cert)
		} else {
			cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
			if err != nil {
				log.Fatalf("can not load x509 key pair; %v", err)
			}
			srv.TLSConfig.Certificates = append(srv.TLSConfig.Certificates, cert)
		}

		srv.Use(s)
		prom.Connections(srv)
		prom.Networks(srv)
		go func() {
			defer wg.Done()

			err = srv.ListenAndServe()
			if err != nil {
				log.Fatalf("can not start server; %v", err)
			}
		}()
	}

	wg.Wait()
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
		return lastBlock.Block, err
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

const promNamespace = "geth_proxy"

var headDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: promNamespace,
	Name:      "head_duration_seconds",
}, []string{})

func promUpdateHeadDuration(ctx context.Context) {
	g, err := headDuration.GetMetricWith(nil)
	if err != nil {
		return
	}

	block, _ := getLastBlock(ctx)
	if block == nil {
		return
	}
	ts := block.Time()
	ts = ts * uint64(blockDuration) // convert to ns
	t := time.Unix(0, int64(ts))
	diff := time.Since(t)

	g.Set(float64(diff) / float64(time.Second))
}
