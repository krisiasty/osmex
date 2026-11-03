package config

import (
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

var fullEnv = map[string]string{
	"OS_AUTH_URL":                      "https://keystone:5000/v3",
	"OS_APPLICATION_CREDENTIAL_ID":     "id",
	"OS_APPLICATION_CREDENTIAL_SECRET": "secret",
}

func TestDefaults(t *testing.T) {
	c, err := Parse(nil, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddress != "127.0.0.1:9180" {
		t.Errorf("listen = %q", c.ListenAddress)
	}
	durations := []struct {
		got, want time.Duration
	}{
		{c.NovaServersInterval, 60 * time.Second},
		{c.NovaLimitsInterval, 300 * time.Second},
		{c.CinderInterval, 60 * time.Second},
		{c.CinderSnapshotsInterval, 60 * time.Second},
		{c.CinderBackupsInterval, 60 * time.Second},
		{c.CinderLimitsInterval, 300 * time.Second},
		{c.NeutronPortsInterval, 60 * time.Second},
		{c.NeutronFIPInterval, 300 * time.Second},
		{c.NeutronLimitsInterval, 300 * time.Second},
		{c.OctaviaInterval, 0},
		{c.OctaviaLimitsInterval, 300 * time.Second},
		{c.BarbicanInterval, 3600 * time.Second},
		{c.RequestTimeout, 30 * time.Second},
	}
	for i, d := range durations {
		if d.got != d.want {
			t.Errorf("duration %d = %v, want %v", i, d.got, d.want)
		}
	}
	if c.OctaviaThresholds != [2]int{20, 100} {
		t.Errorf("thresholds = %v", c.OctaviaThresholds)
	}
	if c.MaxConcurrent != 4 || c.LogLevel != slog.LevelInfo || c.LogFormat != "text" {
		t.Errorf("got %d %v %q", c.MaxConcurrent, c.LogLevel, c.LogFormat)
	}
	if c.AnyService() || c.All || len(c.Services) != 0 {
		t.Errorf("services enabled by default: %+v", c.Services)
	}
	if c.OS.Interface != "public" {
		t.Errorf("interface = %q", c.OS.Interface)
	}
}

func TestServiceFlags(t *testing.T) {
	c, err := Parse([]string{"-octavia", "-nova"}, env(fullEnv))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Services, []string{"nova", "octavia"}) {
		t.Errorf("services = %v", c.Services)
	}
	if !c.AnyService() {
		t.Error("AnyService false")
	}
}

func TestAll(t *testing.T) {
	c, err := Parse([]string{"-all"}, env(fullEnv))
	if err != nil {
		t.Fatal(err)
	}
	if !c.All || !c.AnyService() {
		t.Errorf("all = %v", c.All)
	}
}

func TestExcludeFlags(t *testing.T) {
	noAll := []string{"-all", "-no-nova", "-no-cinder", "-no-neutron", "-no-octavia", "-no-barbican"}
	for _, c := range []struct {
		args                []string
		services, excluded  []string
		unused              []string
		anyService, needEnv bool
	}{
		{[]string{"-all", "-no-barbican"}, nil, []string{"barbican"}, nil, true, true},
		{[]string{"-nova", "-no-nova"}, nil, []string{"nova"}, nil, false, false},
		{[]string{"-no-nova", "-nova"}, nil, []string{"nova"}, nil, false, false},
		{[]string{"-nova", "-cinder", "-no-cinder"}, []string{"nova"}, []string{"cinder"}, nil, true, true},
		{noAll, nil, ServiceNames, nil, false, false},
		{[]string{"-no-nova"}, nil, []string{"nova"}, []string{"nova"}, false, false},
		{[]string{"-nova", "-no-cinder"}, []string{"nova"}, []string{"cinder"}, []string{"cinder"}, true, true},
	} {
		cfg, err := Parse(c.args, env(fullEnv))
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if !slices.Equal(cfg.Services, c.services) || !slices.Equal(cfg.Excluded, c.excluded) || !slices.Equal(cfg.UnusedExclusions, c.unused) ||
			cfg.AnyService() != c.anyService {
			t.Errorf("%v: services %v, excluded %v, unused %v, any %v; want %v, %v, %v, %v",
				c.args, cfg.Services, cfg.Excluded, cfg.UnusedExclusions, cfg.AnyService(), c.services, c.excluded, c.unused, c.anyService)
		}
		// Without an enabled service the OpenStack variables are not needed.
		if _, err := Parse(c.args, env(nil)); (err != nil) != c.needEnv {
			t.Errorf("%v without OS_* variables: err = %v", c.args, err)
		}
	}
}

func TestRequiredEnvWhenServiceEnabled(t *testing.T) {
	for _, k := range []string{"OS_AUTH_URL", "OS_APPLICATION_CREDENTIAL_ID", "OS_APPLICATION_CREDENTIAL_SECRET"} {
		m := map[string]string{}
		for kk, v := range fullEnv {
			if kk != k {
				m[kk] = v
			}
		}
		_, err := Parse([]string{"-nova"}, env(m))
		if err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("missing %s: err = %v", k, err)
		}
	}
	c, err := Parse([]string{"-nova"}, env(fullEnv))
	if err != nil {
		t.Fatal(err)
	}
	if c.OS.AuthURL != fullEnv["OS_AUTH_URL"] || c.OS.CredentialID != "id" || c.OS.CredentialSecret != "secret" {
		t.Errorf("os env = %+v", c.OS)
	}
}

func TestOptionalEnv(t *testing.T) {
	m := map[string]string{"OS_REGION_NAME": "r1", "OS_CACERT": "/ca.pem"}
	maps.Copy(m, fullEnv)
	c, err := Parse([]string{"-nova"}, env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.OS.Region != "r1" || c.OS.CACert != "/ca.pem" {
		t.Errorf("os env = %+v", c.OS)
	}
}

func TestInterfaceNormalisation(t *testing.T) {
	cases := map[string]string{"publicURL": "public", "internalURL": "internal", "adminURL": "admin", "": "public", "internal": "internal"}
	for in, want := range cases {
		c, err := Parse(nil, env(map[string]string{"OS_INTERFACE": in}))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if c.OS.Interface != want {
			t.Errorf("%q → %q, want %q", in, c.OS.Interface, want)
		}
	}
	if _, err := Parse(nil, env(map[string]string{"OS_INTERFACE": "bogus"})); err == nil {
		t.Error("bogus interface accepted")
	}
}

func TestThresholdsParse(t *testing.T) {
	c, err := Parse([]string{"-octavia-auto-thresholds=10,50"}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.OctaviaThresholds != [2]int{10, 50} {
		t.Errorf("thresholds = %v", c.OctaviaThresholds)
	}
	for _, bad := range []string{"50,10", "0,5", "x", "1,2,3", "5"} {
		if _, err := Parse([]string{"-octavia-auto-thresholds=" + bad}, env(nil)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestInvalidValues(t *testing.T) {
	for _, args := range [][]string{
		{"-nova-servers-interval=0"},
		{"-barbican-interval=-1s"},
		{"-octavia-interval=-1s"},
		{"-max-concurrent-requests=0"},
		{"-request-timeout=0"},
		{"-log-format=xml"},
		{"-log-level=loud"},
		{"-no-such-flag"},
		{"extra-arg"},
	} {
		if _, err := Parse(args, env(nil)); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if _, err := Parse([]string{"-octavia-interval=0", "-log-level=debug", "-log-format=json"}, env(nil)); err != nil {
		t.Errorf("valid values rejected: %v", err)
	}
}

func TestDebugFlag(t *testing.T) {
	// -debug logs every HTTP exchange at debug level, so it raises the log level whatever -log-level says.
	for _, args := range [][]string{{"-debug"}, {"-debug", "-log-level", "warn"}, {"-log-level", "error", "-debug"}} {
		c, err := Parse(args, env(nil))
		if err != nil {
			t.Fatal(err)
		}
		if !c.Debug || c.LogLevel != slog.LevelDebug {
			t.Errorf("%v: debug %v, level %v", args, c.Debug, c.LogLevel)
		}
	}
	c, err := Parse([]string{"-log-level", "debug"}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Debug {
		t.Error("-log-level debug enabled HTTP exchange logging")
	}
}

func TestVersionFlag(t *testing.T) {
	c, err := Parse([]string{"-version", "-nova"}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !c.ShowVersion {
		t.Error("ShowVersion false")
	}
}

func TestHelp(t *testing.T) {
	_, err := Parse([]string{"-h"}, env(nil))
	var he *HelpError
	if !errors.As(err, &he) || !strings.Contains(he.Usage, "-listen-address") {
		t.Errorf("err = %v", err)
	}
}

func TestRequireHTTPS(t *testing.T) {
	plain := map[string]string{"OS_APPLICATION_CREDENTIAL_ID": "id", "OS_APPLICATION_CREDENTIAL_SECRET": "secret"}
	for _, u := range []string{"http://keystone:5000/v3", "HTTP://keystone:5000/v3", "keystone:5000/v3"} {
		plain["OS_AUTH_URL"] = u
		_, err := Parse([]string{"-nova"}, env(plain))
		if err == nil || !strings.Contains(err.Error(), "https") || !strings.Contains(err.Error(), "-insecure") {
			t.Errorf("%s: err = %v", u, err)
		}
	}

	plain["OS_AUTH_URL"] = "http://keystone:5000/v3"
	c, err := Parse([]string{"-nova", "-insecure"}, env(plain))
	if err != nil || !c.Insecure {
		t.Errorf("-insecure: insecure %v, err %v", c.Insecure, err)
	}
	if _, err := Parse(nil, env(plain)); err != nil {
		t.Errorf("no services enabled: %v", err)
	}
	plain["OS_AUTH_URL"] = "https://keystone:5000/v3"
	if c, err := Parse([]string{"-nova"}, env(plain)); err != nil || c.Insecure {
		t.Errorf("https: insecure %v, err %v", c.Insecure, err)
	}
}

func TestCinderCopyIntervalsFollowVolumes(t *testing.T) {
	c, err := Parse([]string{"-cinder-interval=10m"}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.CinderSnapshotsInterval != 10*time.Minute || c.CinderBackupsInterval != 10*time.Minute {
		t.Errorf("snapshots %v, backups %v; want both to follow -cinder-interval", c.CinderSnapshotsInterval, c.CinderBackupsInterval)
	}
	c, err = Parse([]string{"-cinder-interval=10m", "-cinder-backups-interval=1h"}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.CinderSnapshotsInterval != 10*time.Minute || c.CinderBackupsInterval != time.Hour {
		t.Errorf("snapshots %v, backups %v", c.CinderSnapshotsInterval, c.CinderBackupsInterval)
	}
	if _, err := Parse([]string{"-cinder-snapshots-interval=-1s"}, env(nil)); err == nil {
		t.Error("negative snapshots interval accepted")
	}
}

func TestListenFlags(t *testing.T) {
	c, err := Parse([]string{
		"-probe-address=:9181",
		"-listen-tls-cert-file=/c", "-listen-tls-key-file=/k", "-listen-tls-client-ca-file=/ca",
		"-listen-auth-username=prometheus", "-listen-auth-password-file=/pw",
	}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.ProbeAddress != ":9181" || c.ListenTLSCertFile != "/c" || c.ListenTLSKeyFile != "/k" || c.ListenTLSClientCAFile != "/ca" ||
		c.ListenAuthUsername != "prometheus" || c.ListenAuthPasswordFile != "/pw" || !c.ListenTLS() || !c.ListenAuth() {
		t.Errorf("config = %+v", c)
	}
	if c, _ := Parse(nil, env(nil)); c.ProbeAddress != "" || c.ListenTLS() || c.ListenAuth() {
		t.Errorf("defaults: %+v", c)
	}
}

func TestListenFlagsMustFit(t *testing.T) {
	for _, args := range [][]string{
		{"-probe-address=127.0.0.1:9180"},
		{"-listen-address=:9100", "-probe-address=:9100"},
		{"-listen-tls-cert-file=/c"},
		{"-listen-tls-key-file=/k"},
		{"-listen-tls-client-ca-file=/ca"},
		{"-listen-tls-client-ca-file=/ca", "-listen-tls-cert-file=/c"},
		{"-listen-auth-username=prometheus"},
		{"-listen-auth-password-file=/pw"},
	} {
		if _, err := Parse(args, env(nil)); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestParseReadsNoListenFile(t *testing.T) {
	// Files are read when the listener is set up, like the credential secret by LoadSecret.
	if _, err := Parse([]string{"-listen-tls-cert-file=/nonexistent/c", "-listen-tls-key-file=/nonexistent/k",
		"-listen-auth-username=u", "-listen-auth-password-file=/nonexistent/pw"}, env(nil)); err != nil {
		t.Errorf("Parse read a listener file: %v", err)
	}
}
