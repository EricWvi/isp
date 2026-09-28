package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("duration must be a string such as 5m")
	}
	v, err := time.ParseDuration(node.Value)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }
func (d Duration) Value() time.Duration      { return time.Duration(d) }

type Config struct {
	Server      Server      `yaml:"server" json:"server"`
	HealthCheck HealthCheck `yaml:"health_check" json:"health_check"`
	Providers   []Provider  `yaml:"providers" json:"providers"`
}

type Server struct {
	HTTPListen   string `yaml:"http_listen" json:"http_listen"`
	SOCKS5Listen string `yaml:"socks5_listen" json:"socks5_listen"`
	Database     string `yaml:"database" json:"database"`
}

type HealthCheck struct {
	URL              string   `yaml:"url" json:"url"`
	IntervalMin      Duration `yaml:"interval_min" json:"interval_min"`
	IntervalMax      Duration `yaml:"interval_max" json:"interval_max"`
	Timeout          Duration `yaml:"timeout" json:"timeout"`
	FailureThreshold int      `yaml:"failure_threshold" json:"failure_threshold"`
	BackoffMax       Duration `yaml:"backoff_max" json:"backoff_max"`
}

type Provider struct {
	ID      string  `yaml:"id" json:"id"`
	Type    string  `yaml:"type" json:"type"`
	Enabled bool    `yaml:"enabled" json:"enabled"`
	Proxies []Proxy `yaml:"proxies" json:"proxies"`
}

type Proxy struct {
	ID            string `yaml:"id" json:"id"`
	Name          string `yaml:"name" json:"name"`
	Host          string `yaml:"host" json:"host"`
	Port          int    `yaml:"port" json:"port"`
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	Username      string `yaml:"username" json:"username"`
	Password      string `yaml:"password" json:"password"`
	UDPCapability string `yaml:"udp_capability,omitempty" json:"udp_capability"`
}

func Defaults() Config {
	return Config{
		Server:      Server{HTTPListen: "127.0.0.1:8080", SOCKS5Listen: "127.0.0.1:30001", Database: "./data/isp.db"},
		HealthCheck: HealthCheck{URL: "https://www.gstatic.com/generate_204", IntervalMin: Duration(5 * time.Minute), IntervalMax: Duration(8 * time.Minute), Timeout: Duration(15 * time.Second), FailureThreshold: 3, BackoffMax: Duration(time.Hour)},
	}
}

func Decode(data []byte) (Config, error) {
	cfg := Defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode YAML: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("config must contain exactly one YAML document")
		}
		return Config{}, fmt.Errorf("decode YAML: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Decode(data)
}

var stableID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
var dnsLabel = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

func (c Config) Validate() error {
	if err := validateListen(c.Server.HTTPListen, false); err != nil {
		return fmt.Errorf("server.http_listen: %w", err)
	}
	if err := validateListen(c.Server.SOCKS5Listen, true); err != nil {
		return fmt.Errorf("server.socks5_listen: %w", err)
	}
	if strings.TrimSpace(c.Server.Database) == "" {
		return errors.New("server.database must not be empty")
	}
	h := c.HealthCheck
	if !validHTTPURL(h.URL) {
		return errors.New("health_check.url must be an absolute HTTP or HTTPS URL")
	}
	if h.IntervalMin.Value() <= 0 || h.IntervalMax.Value() < h.IntervalMin.Value() {
		return errors.New("health_check intervals must be positive and min <= max")
	}
	if h.Timeout.Value() <= 0 || h.FailureThreshold < 1 || h.BackoffMax.Value() < h.IntervalMax.Value() {
		return errors.New("health_check timeout and threshold must be positive; backoff_max must be >= interval_max")
	}
	providerIDs := map[string]bool{}
	proxyIDs := map[string]bool{}
	for i, p := range c.Providers {
		where := fmt.Sprintf("providers[%d]", i)
		if !stableID.MatchString(p.ID) || providerIDs[p.ID] {
			return fmt.Errorf("%s.id must be unique and use lowercase letters, digits and hyphens", where)
		}
		providerIDs[p.ID] = true
		if p.Type != "proxy-seller" {
			return fmt.Errorf("%s.type %q is unsupported", where, p.Type)
		}
		for j, proxy := range p.Proxies {
			loc := fmt.Sprintf("%s.proxies[%d]", where, j)
			if !stableID.MatchString(proxy.ID) || proxyIDs[proxy.ID] {
				return fmt.Errorf("%s.id must be globally unique and use lowercase letters, digits and hyphens", loc)
			}
			proxyIDs[proxy.ID] = true
			if !validHost(proxy.Host) || proxy.Port < 1 || proxy.Port > 65535 {
				return fmt.Errorf("%s host or port is invalid", loc)
			}
			if proxy.Password != "" && proxy.Username == "" {
				return fmt.Errorf("%s.password requires username", loc)
			}
			if len(proxy.Username) > 255 || len(proxy.Password) > 255 {
				return fmt.Errorf("%s credentials exceed SOCKS5 limit of 255 bytes", loc)
			}
			switch proxy.UDPCapability {
			case "", "unknown", "supported", "unsupported":
			default:
				return fmt.Errorf("%s.udp_capability must be unknown, supported or unsupported", loc)
			}
		}
	}
	return nil
}

func validateListen(addr string, socks bool) error {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil || (host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback())) {
		return errors.New("must be a local host:port address")
	}
	var port int
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil || fmt.Sprintf("%d", port) != portText || port < 1 || port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if socks && port < 30000 {
		return errors.New("SOCKS5 port must be at least 30000")
	}
	return nil
}

func validHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, part := range strings.Split(host, ".") {
		if !dnsLabel.MatchString(part) {
			return false
		}
	}
	return true
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.Fragment == "" && u.Opaque == ""
}

type File struct {
	mu       sync.Mutex
	path     string
	cfg      Config
	revision string
}

var ErrConflict = errors.New("configuration has changed")
var ErrInvalid = errors.New("invalid configuration")
var ErrWrite = errors.New("cannot write configuration")

func Open(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Decode(data)
	if err != nil {
		return nil, err
	}
	return &File{path: path, cfg: cfg, revision: revision(data)}, nil
}

func (f *File) Snapshot() Config {
	cfg, _ := f.SnapshotWithRevision()
	return cfg
}

func (f *File) SnapshotWithRevision() (Config, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return clone(f.cfg), f.revision
}

// Update serializes read-modify-write operations and publishes only after a durable rename.
func (f *File) Update(change func(*Config) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updateLocked(change)
}

func (f *File) UpdateIfRevision(expected string, change func(*Config) error) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if expected == "" || expected != f.revision {
		return f.revision, ErrConflict
	}
	if err := f.updateLocked(change); err != nil {
		return f.revision, err
	}
	return f.revision, nil
}

func (f *File) updateLocked(change func(*Config) error) error {
	next := clone(f.cfg)
	if err := change(&next); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	data, err := writeAtomic(f.path, next)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWrite, err)
	}
	f.cfg = next
	f.revision = revision(data)
	return nil
}

func revision(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func clone(c Config) Config {
	c.Providers = append([]Provider(nil), c.Providers...)
	for i := range c.Providers {
		c.Providers[i].Proxies = append([]Proxy(nil), c.Providers[i].Proxies...)
	}
	return c
}

func writeAtomic(path string, cfg Config) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(cfg); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	data := buffer.Bytes()
	if _, err := Decode(data); err != nil {
		return nil, fmt.Errorf("verify serialized config: %w", err)
	}
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".isp-config-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	mode := os.FileMode(0600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if _, err := Load(file.Name()); err != nil {
		return nil, fmt.Errorf("verify temporary config: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return nil, err
	}
	if d, err := os.Open(dir); err == nil {
		// The replacement is already visible. A directory sync failure cannot
		// be reported as a failed update without diverging from the disk state.
		_ = d.Sync()
		_ = d.Close()
	}
	return data, nil
}
