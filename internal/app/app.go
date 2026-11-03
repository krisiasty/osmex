// Package app wires configuration, OpenStack client, services and HTTP server together.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/krisiasty/osmex/internal/barbican"
	"github.com/krisiasty/osmex/internal/cinder"
	"github.com/krisiasty/osmex/internal/config"
	"github.com/krisiasty/osmex/internal/expo"
	"github.com/krisiasty/osmex/internal/keystone"
	"github.com/krisiasty/osmex/internal/listen"
	"github.com/krisiasty/osmex/internal/neutron"
	"github.com/krisiasty/osmex/internal/nova"
	"github.com/krisiasty/osmex/internal/octavia"
	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/poller"
	"github.com/krisiasty/osmex/internal/runtimestats"
	"github.com/krisiasty/osmex/internal/secret"
	"github.com/krisiasty/osmex/internal/server"
)

// Service is one OpenStack service and the poller modules it contributes.
type Service struct {
	Name    string
	Modules []string // module names Build returns, in order; ?module= accepts them before polling starts
	Spec    osclient.ServiceSpec
	Build   func(svc *osclient.ServiceClient, cfg config.Config, opts ...poller.Option) []*poller.Poller
}

// Services lists the services in canonical order.
var Services = []Service{
	{"nova", []string{"nova-servers", "nova-limits"}, nova.Spec, func(svc *osclient.ServiceClient, cfg config.Config, opts ...poller.Option) []*poller.Poller {
		return []*poller.Poller{nova.NewServers(svc, cfg.NovaServersInterval, opts...), nova.NewLimits(svc, cfg.NovaLimitsInterval, opts...)}
	}},
	{"cinder", []string{"cinder", "cinder-snapshots", "cinder-backups", "cinder-limits"}, cinder.Spec, func(svc *osclient.ServiceClient, cfg config.Config, opts ...poller.Option) []*poller.Poller {
		iv := cinder.Intervals{Volumes: cfg.CinderInterval, Snapshots: cfg.CinderSnapshotsInterval, Backups: cfg.CinderBackupsInterval}
		return append(cinder.Pollers(svc, iv, opts...), cinder.NewLimits(svc, cfg.CinderLimitsInterval, opts...))
	}},
	{"neutron", []string{"neutron-ports", "neutron-fip", "neutron-limits"}, neutron.Spec, func(svc *osclient.ServiceClient, cfg config.Config, opts ...poller.Option) []*poller.Poller {
		return []*poller.Poller{
			neutron.NewPorts(svc, cfg.NeutronPortsInterval, opts...),
			neutron.NewFloatingIPs(svc, cfg.NeutronFIPInterval, opts...),
			neutron.NewLimits(svc, cfg.NeutronLimitsInterval, opts...),
		}
	}},
	{"octavia", []string{"octavia", "octavia-limits"}, octavia.Spec, func(svc *osclient.ServiceClient, cfg config.Config, opts ...poller.Option) []*poller.Poller {
		return []*poller.Poller{
			octavia.New(svc, cfg.OctaviaInterval, cfg.OctaviaThresholds, opts...),
			octavia.NewLimits(svc, cfg.OctaviaLimitsInterval, opts...),
		}
	}},
	{"barbican", []string{"barbican"}, barbican.Spec, func(svc *osclient.ServiceClient, cfg config.Config, opts ...poller.Option) []*poller.Poller {
		return []*poller.Poller{barbican.New(svc, cfg.BarbicanInterval, opts...)}
	}},
}

// Options carries process-level settings for Run.
type Options struct {
	Version       string
	Logger        *slog.Logger
	Listener      net.Listener // nil → listen on cfg.ListenAddress
	ProbeListener net.Listener // nil → listen on cfg.ProbeAddress, if set
}

const shutdownTimeout = 10 * time.Second

// newServer returns an HTTP server with timeouts, so a slow client cannot hold a connection open.
func newServer(h http.Handler, tlsCfg *tls.Config) *http.Server {
	return &http.Server{Handler: h, TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
}

// secureListener returns the TLS configuration for the main listener, nil without TLS, and a function that adds
// basic authentication to a handler when it is configured. It reads every file now, so a wrong path fails at startup
// rather than at the first scrape.
func secureListener(cfg config.Config, log *slog.Logger) (*tls.Config, func(http.Handler) http.Handler, error) {
	tlsCfg, err := listen.TLSConfig(listen.Options{CertFile: cfg.ListenTLSCertFile, KeyFile: cfg.ListenTLSKeyFile, ClientCAFile: cfg.ListenTLSClientCAFile})
	if err != nil {
		return nil, nil, fmt.Errorf("listener TLS: %w", err)
	}
	protect := func(h http.Handler) http.Handler { return h }
	if cfg.ListenAuth() {
		password, err := secret.NewFileCredential(cfg.ListenAuthPasswordFile)
		if err != nil {
			return nil, nil, fmt.Errorf("-listen-auth-password-file: %w", err)
		}
		protect = func(h http.Handler) http.Handler {
			return listen.BasicAuth(h, cfg.ListenAuthUsername, password, log, server.ProbePaths...)
		}
		if !cfg.ListenTLS() {
			log.Warn("basic authentication without TLS sends the password in clear on every scrape")
		}
	}
	for flag, path := range map[string]string{"-listen-tls-key-file": cfg.ListenTLSKeyFile, "-listen-auth-password-file": cfg.ListenAuthPasswordFile} {
		if path == "" {
			continue
		}
		if info, err := os.Stat(path); err == nil && secret.LooseMode(info) {
			log.Warn("secret file readable beyond its owner", "flag", flag, "path", path, "mode", fmt.Sprintf("%#o", info.Mode().Perm()))
		}
	}
	return tlsCfg, protect, nil
}

// selectServices picks the services -all enables: those with an endpoint for the configured region and
// interface, found the same way the client finds it. A service that has none is skipped. A service whose region is
// ambiguous (endpoints in several regions and no OS_REGION_NAME) is a configuration mistake, so the service is kept
// and its modules fail visibly through api_up, and an error is logged.
func selectServices(catalog osclient.Catalog, region, iface string, excluded []string, log *slog.Logger) []Service {
	var services []Service
	for _, m := range Services {
		if slices.Contains(excluded, m.Name) {
			log.Info("service excluded", "service", m.Name)
			continue
		}
		_, err := catalog.EndpointURL(m.Spec.Types, region, iface)
		switch {
		case err == nil:
			services = append(services, m)
		case errors.Is(err, osclient.ErrNoEndpoint):
			log.Info("service skipped: no endpoint for the configured region and interface", "service", m.Name, "err", err)
		default:
			log.Error("service enabled, but its endpoint is ambiguous", "service", m.Name, "err", err)
			services = append(services, m)
		}
	}
	return services
}

// redactURL returns u without user information, query or fragment, so it is safe to log.
func redactURL(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return "(unparsable URL)"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// Run serves metrics until ctx is done. It returns an error only for startup failures (unusable
// OS_CACERT, listen failure) or if the HTTP server stops unexpectedly; OpenStack problems are
// reported through metrics instead.
func Run(ctx context.Context, cfg config.Config, o Options) error {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	httpClient, err := osclient.NewHTTPClient(cfg.OS.CACert)
	if err != nil {
		return err
	}
	if cfg.Debug {
		httpClient.Transport = osclient.NewLoggingTransport(httpClient.Transport, log)
	}

	var pollers atomic.Pointer[[]*poller.Poller]
	pollers.Store(&[]*poller.Poller{})
	// settled is set once the pollers are started, or under -all once the first authentication has failed: readiness
	// must not wait for a Keystone that may never answer, or auth_up = 0 would be hidden from a scraper that skips
	// endpoints that are not ready.
	var settled atomic.Bool
	health := server.NewHealth(func() []*poller.Poller { return *pollers.Load() }, settled.Load, nil)
	srvOpts := server.Options{
		Pollers:   func() []*poller.Poller { return *pollers.Load() },
		Version:   o.Version,
		GoVersion: runtime.Version(),
		Runtime:   func(p *expo.Page) { runtimestats.Render(p, os.DirFS("/")) },
		Health:    health,
	}
	tlsCfg, protect, err := secureListener(cfg, log)
	if err != nil {
		return err
	}

	ln := o.Listener
	if ln == nil {
		if ln, err = (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddress); err != nil {
			return err
		}
	}
	probeLn := o.ProbeListener
	if probeLn == nil && cfg.ProbeAddress != "" {
		if probeLn, err = (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ProbeAddress); err != nil {
			_ = ln.Close()
			return err
		}
	}

	// Deferred calls run in reverse: cancel the pollers first, then wait for them.
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if cfg.AnyService() {
		userAgent := "osmex/" + o.Version
		auth := osclient.NewAuth(osclient.AuthConfig{
			URL:              cfg.OS.AuthURL,
			CredentialID:     cfg.OS.CredentialID,
			CredentialSecret: cfg.OS.CredentialSecret,
			HTTP:             httpClient,
			UserAgent:        userAgent,
			Timeout:          cfg.RequestTimeout,
			AllowInsecure:    cfg.Insecure,
			Logger:           log,
		})
		srvOpts.AuthUp = auth.Up
		client := osclient.NewClient(osclient.ClientConfig{
			Auth:           auth,
			HTTP:           httpClient,
			Region:         cfg.OS.Region,
			Interface:      cfg.OS.Interface,
			UserAgent:      userAgent,
			MaxConcurrent:  cfg.MaxConcurrent,
			RequestTimeout: cfg.RequestTimeout,
			AllowInsecure:  cfg.Insecure,
		})
		if cfg.Insecure {
			log.Warn("unencrypted connections to OpenStack are allowed (-insecure)")
		}
		// Name the target before contacting it, so a failing authentication still shows where it went.
		log.Info("connecting to cloud", "auth_url", redactURL(cfg.OS.AuthURL), "region", cfg.OS.Region, "interface", cfg.OS.Interface)
		wg.Go(func() { auth.Run(ctx) })
		wg.Go(func() {
			select {
			case <-auth.Ready():
				p := auth.Project()
				log.Info("connected to cloud", "project", p.Name, "project_id", p.ID, "domain", p.DomainName, "domain_id", p.DomainID)
			case <-ctx.Done():
			}
		})

		// The credential module is shared: it runs whenever any service is enabled.
		srvOpts.Modules = append(srvOpts.Modules, keystone.CredentialPoller)
		startServices := func(services []Service) {
			ps := []*poller.Poller{keystone.NewCredential(client.Service(keystone.Spec), cfg.OS.CredentialID, keystone.CredentialInterval, poller.WithLogger(log))}
			for _, m := range services {
				ps = append(ps, m.Build(client.Service(m.Spec), cfg, poller.WithLogger(log))...)
			}
			pollers.Store(&ps)
			settled.Store(true)
			for _, p := range ps {
				wg.Go(func() { p.Run(ctx) })
			}
			log.Info("modules started", "count", len(ps))
		}

		if cfg.All {
			// Any service not excluded may start once the catalog is known, so ?module= accepts all of their modules from the start.
			for _, m := range Services {
				if !slices.Contains(cfg.Excluded, m.Name) {
					srvOpts.Modules = append(srvOpts.Modules, m.Modules...)
				}
			}
			wg.Go(func() {
				select {
				case <-auth.Ready():
				case <-ctx.Done():
					return
				}
				// The catalog of the token that closed Ready; asking for a fresh token here
				// could fail and would leave the exporter without pollers.
				startServices(selectServices(auth.Catalog(), cfg.OS.Region, cfg.OS.Interface, cfg.Excluded, log))
			})
			wg.Go(func() {
				select {
				case <-auth.Attempted():
				case <-ctx.Done():
					return
				}
				select {
				case <-auth.Ready(): // succeeded: settled once the services start
				default:
					settled.Store(true)
				}
			})
		} else {
			var services []Service
			for _, m := range Services {
				if slices.Contains(cfg.Services, m.Name) {
					services = append(services, m)
					srvOpts.Modules = append(srvOpts.Modules, m.Modules...)
				}
			}
			startServices(services)
		}
	}

	if !cfg.AnyService() {
		settled.Store(true)
	}

	srv := newServer(protect(server.Handler(srvOpts)), tlsCfg)
	servers := []*http.Server{srv}
	serveErr := make(chan error, 2)
	go func() {
		if tlsCfg != nil {
			// The certificate comes from tlsCfg, which reloads it, rather than from file names here.
			serveErr <- srv.ServeTLS(ln, "", "")
			return
		}
		serveErr <- srv.Serve(ln)
	}()
	log.Info("serving metrics", "address", ln.Addr().String(), "tls", cfg.ListenTLS(), "client_certificate", cfg.ListenTLSClientCAFile != "",
		"auth", cfg.ListenAuth())
	if probeLn != nil {
		probeSrv := newServer(server.ProbeHandler(health), nil)
		servers = append(servers, probeSrv)
		go func() { serveErr <- probeSrv.Serve(probeLn) }()
		log.Info("serving probes", "address", probeLn.Addr().String())
	}

	select {
	case err := <-serveErr:
		for _, s := range servers {
			_ = s.Close()
		}
		return err
	case <-ctx.Done():
	}
	// ctx is already cancelled here; shut down on a context that keeps its values but not its cancellation.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancelShutdown()
	var errs []error
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
