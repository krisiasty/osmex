// Package config parses osmex command-line flags and OpenStack environment variables.
package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ServiceNames lists the services in canonical order.
var ServiceNames = []string{"nova", "cinder", "neutron", "octavia", "barbican"}

// OSEnv holds the OpenStack environment variables from an application-credential openrc.
type OSEnv struct {
	AuthURL              string
	CredentialID         string
	CredentialSecret     string // inline, or read by LoadSecret
	CredentialSecretFile string // OS_APPLICATION_CREDENTIAL_SECRET_FILE
	CredentialsDirectory string // CREDENTIALS_DIRECTORY, set by systemd for a unit that loads credentials
	Region               string
	Interface            string
	CACert               string
}

// Config is the complete exporter configuration.
type Config struct {
	ListenAddress           string
	ProbeAddress            string // "" = the probes are served on ListenAddress only
	ListenTLSCertFile       string
	ListenTLSKeyFile        string
	ListenTLSClientCAFile   string
	ListenAuthUsername      string
	ListenAuthPasswordFile  string
	Services                []string // explicitly enabled services, in ServiceNames order, without excluded ones
	All                     bool
	Excluded                []string // services excluded with -no-<service>, in ServiceNames order; they win over -<service> and -all
	UnusedExclusions        []string // excluded services that neither -all nor -<service> enabled
	NovaServersInterval     time.Duration
	NovaLimitsInterval      time.Duration
	CinderInterval          time.Duration
	CinderSnapshotsInterval time.Duration // 0 on the command line = CinderInterval
	CinderBackupsInterval   time.Duration // 0 on the command line = CinderInterval
	CinderLimitsInterval    time.Duration
	NeutronPortsInterval    time.Duration
	NeutronFIPInterval      time.Duration
	NeutronLimitsInterval   time.Duration
	OctaviaInterval         time.Duration // 0 = auto
	OctaviaThresholds       [2]int
	OctaviaLimitsInterval   time.Duration
	BarbicanInterval        time.Duration
	MaxConcurrent           int
	RequestTimeout          time.Duration
	LogLevel                slog.Level
	LogFormat               string // "text" or "json"
	Debug                   bool   // log every HTTP exchange; implies LogLevel debug
	ShowVersion             bool
	Insecure                bool // allow plain-http connections to OpenStack
	OS                      OSEnv
}

// ListenTLS reports whether /metrics is served over HTTPS.
func (c Config) ListenTLS() bool { return c.ListenTLSCertFile != "" }

// ListenAuth reports whether /metrics requires basic authentication.
func (c Config) ListenAuth() bool { return c.ListenAuthUsername != "" }

// AnyService reports whether at least one service is enabled.
func (c Config) AnyService() bool {
	return len(c.Services) > 0 || c.All && len(c.Excluded) < len(ServiceNames)
}

// HelpError is returned when -h or -help is given; Usage holds the flag documentation.
type HelpError struct{ Usage string }

func (e *HelpError) Error() string { return "help requested" }

// Parse parses args (without the program name) and reads OpenStack variables through getenv. It reads no files: when a
// service is enabled, LoadSecret must run next to resolve the application credential secret.
func Parse(args []string, getenv func(string) string) (Config, error) {
	var c Config
	fs := flag.NewFlagSet("osmex", flag.ContinueOnError)
	var usage bytes.Buffer
	fs.SetOutput(&usage)

	enabled := make(map[string]*bool, len(ServiceNames))
	excluded := make(map[string]*bool, len(ServiceNames))
	for _, m := range ServiceNames {
		enabled[m] = fs.Bool(m, false, "enable the "+m+" service")
		excluded[m] = fs.Bool("no-"+m, false, "exclude the "+m+" service, even with -all or -"+m)
	}
	fs.BoolVar(&c.All, "all", false, "enable every service available in the catalog")
	fs.StringVar(&c.ListenAddress, "listen-address", "127.0.0.1:9180", "address to serve /metrics, /livez and /readyz on")
	fs.StringVar(&c.ProbeAddress, "probe-address", "", "also serve /livez and /readyz, and nothing else, on this address over plain HTTP without authentication")
	fs.StringVar(&c.ListenTLSCertFile, "listen-tls-cert-file", "", "serve HTTPS with this PEM certificate; re-read when it changes")
	fs.StringVar(&c.ListenTLSKeyFile, "listen-tls-key-file", "", "PEM private key for -listen-tls-cert-file")
	fs.StringVar(&c.ListenTLSClientCAFile, "listen-tls-client-ca-file", "", "require a client certificate signed by this PEM bundle")
	fs.StringVar(&c.ListenAuthUsername, "listen-auth-username", "", "require basic authentication with this username, except for /livez and /readyz")
	fs.StringVar(&c.ListenAuthPasswordFile, "listen-auth-password-file", "", "file holding the basic authentication password; re-read after a rejection")
	fs.DurationVar(&c.NovaServersInterval, "nova-servers-interval", 60*time.Second, "nova-servers poll interval")
	fs.DurationVar(&c.NovaLimitsInterval, "nova-limits-interval", 300*time.Second, "nova-limits poll interval")
	fs.DurationVar(&c.CinderInterval, "cinder-interval", 60*time.Second, "cinder poll interval (volumes)")
	fs.DurationVar(&c.CinderSnapshotsInterval, "cinder-snapshots-interval", 0, "cinder-snapshots poll interval; 0 = same as -cinder-interval")
	fs.DurationVar(&c.CinderBackupsInterval, "cinder-backups-interval", 0, "cinder-backups poll interval; 0 = same as -cinder-interval")
	fs.DurationVar(&c.CinderLimitsInterval, "cinder-limits-interval", 300*time.Second, "cinder-limits poll interval")
	fs.DurationVar(&c.NeutronPortsInterval, "neutron-ports-interval", 60*time.Second, "neutron-ports poll interval")
	fs.DurationVar(&c.NeutronFIPInterval, "neutron-fip-interval", 300*time.Second, "neutron-fip poll interval")
	fs.DurationVar(&c.NeutronLimitsInterval, "neutron-limits-interval", 300*time.Second, "neutron-limits poll interval")
	fs.DurationVar(&c.OctaviaInterval, "octavia-interval", 0, "octavia poll interval; 0 scales with the load balancer count")
	thresholds := fs.String("octavia-auto-thresholds", "20,100", "load balancer counts at which the auto octavia interval rises to 120s and 300s")
	fs.DurationVar(&c.OctaviaLimitsInterval, "octavia-limits-interval", 300*time.Second, "octavia-limits poll interval")
	fs.DurationVar(&c.BarbicanInterval, "barbican-interval", 3600*time.Second, "barbican poll interval")
	fs.IntVar(&c.MaxConcurrent, "max-concurrent-requests", 4, "maximum concurrent OpenStack API requests")
	fs.DurationVar(&c.RequestTimeout, "request-timeout", 30*time.Second, "timeout for a single HTTP request")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	fs.StringVar(&c.LogFormat, "log-format", "text", "text or json")
	fs.BoolVar(&c.Debug, "debug", false, "log every HTTP exchange with OpenStack (implies -log-level debug)")
	fs.BoolVar(&c.ShowVersion, "version", false, "print version and exit")
	fs.BoolVar(&c.Insecure, "insecure", false,
		"allow unencrypted (plain http) connections to Keystone and service endpoints; https certificates are still verified")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return c, &HelpError{Usage: usage.String()}
		}
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if c.ShowVersion {
		return c, nil
	}
	for _, m := range ServiceNames {
		switch {
		case *excluded[m]:
			c.Excluded = append(c.Excluded, m)
			if !c.All && !*enabled[m] {
				c.UnusedExclusions = append(c.UnusedExclusions, m)
			}
		case *enabled[m]:
			c.Services = append(c.Services, m)
		}
	}

	for name, d := range map[string]time.Duration{
		"nova-servers-interval":   c.NovaServersInterval,
		"nova-limits-interval":    c.NovaLimitsInterval,
		"cinder-interval":         c.CinderInterval,
		"cinder-limits-interval":  c.CinderLimitsInterval,
		"neutron-ports-interval":  c.NeutronPortsInterval,
		"neutron-fip-interval":    c.NeutronFIPInterval,
		"neutron-limits-interval": c.NeutronLimitsInterval,
		"octavia-limits-interval": c.OctaviaLimitsInterval,
		"barbican-interval":       c.BarbicanInterval,
		"request-timeout":         c.RequestTimeout,
	} {
		if d <= 0 {
			return c, fmt.Errorf("-%s must be positive", name)
		}
	}
	for name, d := range map[string]*time.Duration{
		"cinder-snapshots-interval": &c.CinderSnapshotsInterval,
		"cinder-backups-interval":   &c.CinderBackupsInterval,
	} {
		switch {
		case *d < 0:
			return c, fmt.Errorf("-%s must not be negative", name)
		case *d == 0:
			*d = c.CinderInterval
		}
	}
	if c.OctaviaInterval < 0 {
		return c, errors.New("-octavia-interval must not be negative")
	}
	var err error
	if c.OctaviaThresholds, err = parseThresholds(*thresholds); err != nil {
		return c, err
	}
	if err := c.validateListen(); err != nil {
		return c, err
	}
	if c.MaxConcurrent < 1 {
		return c, errors.New("-max-concurrent-requests must be at least 1")
	}
	if err := c.LogLevel.UnmarshalText([]byte(*logLevel)); err != nil {
		return c, fmt.Errorf("-log-level: %w", err)
	}
	if c.Debug {
		c.LogLevel = slog.LevelDebug
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		return c, fmt.Errorf("-log-format must be text or json, got %q", c.LogFormat)
	}

	c.OS = OSEnv{
		AuthURL:              getenv("OS_AUTH_URL"),
		CredentialID:         getenv("OS_APPLICATION_CREDENTIAL_ID"),
		CredentialSecret:     getenv("OS_APPLICATION_CREDENTIAL_SECRET"),
		CredentialSecretFile: getenv("OS_APPLICATION_CREDENTIAL_SECRET_FILE"),
		CredentialsDirectory: getenv("CREDENTIALS_DIRECTORY"),
		Region:               getenv("OS_REGION_NAME"),
		CACert:               getenv("OS_CACERT"),
	}
	if c.OS.Interface, err = normaliseInterface(getenv("OS_INTERFACE")); err != nil {
		return c, err
	}
	if c.AnyService() {
		for name, v := range map[string]string{
			"OS_AUTH_URL":                  c.OS.AuthURL,
			"OS_APPLICATION_CREDENTIAL_ID": c.OS.CredentialID,
		} {
			if v == "" {
				return c, fmt.Errorf("%s must be set when a service is enabled", name)
			}
		}
		if u, err := url.Parse(c.OS.AuthURL); !c.Insecure && (err != nil || !strings.EqualFold(u.Scheme, "https")) {
			scheme := "unparsable"
			if err == nil {
				scheme = u.Scheme
			}
			return c, fmt.Errorf("OS_AUTH_URL must be an https URL (got scheme %q); use -insecure to allow unencrypted connections", scheme)
		}
		// The secret itself is read by LoadSecret; only what needs no file access is checked here.
		switch {
		case c.OS.CredentialSecret != "" && c.OS.CredentialSecretFile != "":
			return c, errBothSecrets
		case c.OS.CredentialSecret == "" && c.OS.CredentialSecretFile == "" && c.OS.CredentialsDirectory == "":
			return c, errNoSecret
		}
	}
	return c, nil
}

// validateListen checks that the listener flags fit together. It reads no files: they are checked when the listener
// is set up.
func (c Config) validateListen() error {
	switch {
	case c.ProbeAddress != "" && c.ProbeAddress == c.ListenAddress:
		return errors.New("-probe-address must differ from -listen-address")
	case (c.ListenTLSCertFile == "") != (c.ListenTLSKeyFile == ""):
		return errors.New("set both -listen-tls-cert-file and -listen-tls-key-file, or neither")
	case c.ListenTLSClientCAFile != "" && !c.ListenTLS():
		return errors.New("-listen-tls-client-ca-file requires -listen-tls-cert-file and -listen-tls-key-file")
	case (c.ListenAuthUsername == "") != (c.ListenAuthPasswordFile == ""):
		return errors.New("set both -listen-auth-username and -listen-auth-password-file, or neither")
	}
	return nil
}

func parseThresholds(s string) ([2]int, error) {
	var t [2]int
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return t, fmt.Errorf("-octavia-auto-thresholds must be two comma-separated counts, got %q", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return t, fmt.Errorf("-octavia-auto-thresholds: %w", err)
		}
		t[i] = n
	}
	if t[0] < 1 || t[1] <= t[0] {
		return t, fmt.Errorf("-octavia-auto-thresholds must satisfy 0 < first < second, got %q", s)
	}
	return t, nil
}

func normaliseInterface(s string) (string, error) {
	s = strings.TrimSuffix(s, "URL")
	switch s {
	case "":
		return "public", nil
	case "public", "internal", "admin":
		return s, nil
	}
	return "", fmt.Errorf("OS_INTERFACE must be public, internal or admin, got %q", s)
}
