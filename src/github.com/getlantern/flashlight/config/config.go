package config

import (
	"compress/gzip"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"code.google.com/p/go-uuid/uuid"

	"github.com/getlantern/appdir"
	"github.com/getlantern/fronted"
	"github.com/getlantern/golog"
	"github.com/getlantern/jibber_jabber"
	"github.com/getlantern/launcher"
	"github.com/getlantern/proxiedsites"
	"github.com/getlantern/yaml"
	"github.com/getlantern/yamlconf"

	"github.com/getlantern/flashlight/client"
	"github.com/getlantern/flashlight/globals"
	"github.com/getlantern/flashlight/server"
	"github.com/getlantern/flashlight/statreporter"
)

const (
	CloudConfigPollInterval = 1 * time.Minute
	cloudflare              = "cloudflare"
	etag                    = "X-Lantern-Etag"
	ifNoneMatch             = "X-Lantern-If-None-Match"
	//	defaultCloudConfigUrl   = "http://config.getiantem.org/cloud.yaml.gz"

	// This is over HTTP because proxies do not forward X-Forwarded-For with HTTPS.
	defaultCloudConfigUrl = "http://d2wi0vwulmtn99.cloudfront.net/cloud.yaml.gz"
)

var (
	log                 = golog.LoggerFor("flashlight.config")
	m                   *yamlconf.Manager
	lastCloudConfigETag = map[string]string{}
	httpClient          atomic.Value
	r                   = regexp.MustCompile("\\d+\\.\\d+")
)

type Config struct {
	Version       int
	CloudConfig   string
	CloudConfigCA string
	Addr          string
	Role          string
	InstanceId    string
	CpuProfile    string
	MemProfile    string
	UIAddr        string // UI HTTP server address
	AutoReport    *bool  // Report anonymous usage to GA
	AutoLaunch    *bool  // Automatically launch Lantern on system startup
	Stats         *statreporter.Config
	Server        *server.ServerConfig
	Client        *client.ClientConfig
	ProxiedSites  *proxiedsites.Config // List of proxied site domains that get routed through Lantern rather than accessed directly
	TrustedCAs    []*CA
}

func Configure(c *http.Client) {
	httpClient.Store(c)
	// No-op if already started.
	m.StartPolling()
}

// CA represents a certificate authority
type CA struct {
	CommonName string
	Cert       string // PEM-encoded
}

func exists(file string) (os.FileInfo, bool) {
	if fi, err := os.Stat(file); os.IsNotExist(err) {
		log.Debugf("File does not exist at %v", file)
		return fi, false
	} else {
		log.Debugf("File exists at %v", file)
		return fi, true
	}
}

// hasCustomChainedServer returns whether or not the config file at the specified
// path includes a custom chained server or not.
func hasCustomChainedServer(configPath, name string) bool {
	if !(strings.HasPrefix(name, "lantern") && strings.HasSuffix(name, ".yaml")) {
		log.Debugf("File name does not match")
		return false
	}
	bytes, err := ioutil.ReadFile(configPath)
	if err != nil {
		log.Errorf("Could not read file %v", err)
		return false
	}
	cfg := &Config{}
	err = yaml.Unmarshal(bytes, cfg)
	if err != nil {
		log.Errorf("Could not unmarshal config %v", err)
		return false
	}

	nc := len(cfg.Client.ChainedServers)

	log.Debugf("Found %v chained servers", nc)
	// The config will have more than one but fewer than 10 chained servers
	// if it has been given a custom config with a custom chained server
	// list
	return nc > 0 && nc < 10
}

func isGoodConfig(configPath string) bool {
	log.Debugf("Checking config path: %v", configPath)
	fi, exists := exists(configPath)
	return exists && hasCustomChainedServer(configPath, fi.Name())
}

func majorVersion(version string) string {
	return r.FindString(version)
}

// copyGoodOldConfig is a one-time function for using older config files in the 2.x series.
func copyGoodOldConfig(configDir, configPath string) {
	// If we already have a config file with the latest name, use that one.
	// Otherwise, copy the most recent config file available.
	exists := isGoodConfig(configPath)
	if exists {
		log.Debugf("Using existing config")
		return
	}

	files, err := ioutil.ReadDir(configDir)
	if err != nil {
		log.Errorf("Could not read config dir: %v", err)
		return
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}
		name := file.Name()
		path := filepath.Join(configDir, name)
		if isGoodConfig(path) {
			// Just use the old config since configs in the 2.x series haven't changed.
			if err := os.Rename(path, configPath); err != nil {
				log.Errorf("Could not rename file from %v to %v: %v", path, configPath, err)
			} else {
				log.Debugf("Copied old config at %v to %v", path, configPath)
				return
			}
		}
	}
	return
}

// Init initializes the configuration system.
func Init(version string) (*Config, error) {
	file := "lantern-" + version + ".yaml"
	configDir, configPath, err := InConfigDir(file)
	if err != nil {
		log.Errorf("Could not get config path? %v", err)
		return nil, err
	}
	copyGoodOldConfig(configDir, configPath)

	m = &yamlconf.Manager{
		FilePath:         configPath,
		FilePollInterval: 1 * time.Second,
		EmptyConfig: func() yamlconf.Config {
			return &Config{}
		},
		FirstRunSetup: func(ycfg yamlconf.Config) error {
			log.Debugf("Running first run setup")
			cfg := ycfg.(*Config)
			servers, err := client.ReadChained()
			if err != nil {
				// Let the bootstrap code itself log errors as necessary.
				// This could happen if we're auto-updated from an older version that didn't
				// have packaged settings, for example, but those versions should all
				// have working configs!!
				log.Errorf("Could not read yaml: %v", err)
				return err
			}
			log.Debugf("Bootstrap settings has %v chained servers", len(servers.ChainedServers))
			configs := make(chan []byte, 1)

			var once sync.Once
			url := cfg.CloudConfig
			bootstrapConfig(servers, configs, &once, url)

			// Simultaneously try to get the config using direct domain fronting.
			go func() {
				certs := trustedCACerts()
				direct, err := fronted.NewDirect(certs, cloudfrontMasquerades)
				if err != nil {
					log.Errorf("Could not create direct domain fronter")
				} else {
					if resp, err := direct.Response(url); err != nil {
						log.Errorf("Could not get response %v", err)
					} else {
						log.Debugf("Got response with direct domain fronter")
						if body, err := readConfigResponse(url, resp); err != nil {
							log.Errorf("Error reading response body? %v", err)
						} else {
							once.Do(func() { configs <- body })
						}
					}
				}
			}()

			config := <-configs
			log.Debugf("Read config")
			return cfg.updateFrom(config)
		},
		PerSessionSetup: func(ycfg yamlconf.Config) error {
			cfg := ycfg.(*Config)
			return cfg.applyFlags()
		},
		CustomPoll: func(currentCfg yamlconf.Config) (mutate func(yamlconf.Config) error, waitTime time.Duration, err error) {
			return pollWithHttpClient(currentCfg, httpClient.Load().(*http.Client))
		},
	}
	initial, err := m.Init()

	var cfg *Config
	if err != nil {
		log.Errorf("Error initializing config: %v", err)
	} else {
		cfg = initial.(*Config)
		err = updateGlobals(cfg)
		if err != nil {
			return nil, err
		}
	}
	return cfg, err
}

func bootstrapConfig(bs *client.BootstrapServers, configs chan []byte, once *sync.Once, url string) {
	for _, s := range bs.ChainedServers {
		go func(s *client.ChainedServerInfo) {
			bootstrap(s, configs, once, url)
		}(s)
	}
}

func bootstrap(s *client.ChainedServerInfo, configs chan []byte, once *sync.Once, url string) {
	log.Debugf("Fetching config using chained server: %v", s.Addr)
	dialer, er := s.Dialer()
	if er != nil {
		log.Errorf("Unable to configure chained server. Received error: %v", er)
		return
	}
	client := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives:   true,
			Dial:                dialer.Dial,
			TLSHandshakeTimeout: 30 * time.Second,
		},
	}
	if bytes, err := fetchCloudConfig(client, url, s.AuthToken); err == nil {
		log.Debugf("Successfully downloaded custom config")

		// We just use the first config we learn about.
		once.Do(func() { configs <- bytes })
	}
}

func pollWithHttpClient(currentCfg yamlconf.Config, client *http.Client) (mutate func(yamlconf.Config) error, waitTime time.Duration, err error) {
	// By default, do nothing
	mutate = func(ycfg yamlconf.Config) error {
		// do nothing
		return nil
	}
	cfg := currentCfg.(*Config)
	waitTime = cfg.cloudPollSleepTime()
	if cfg.CloudConfig == "" {
		// Config doesn't have a CloudConfig, just ignore
		return mutate, waitTime, nil
	}

	url := cfg.CloudConfig

	// We don't pass an auth token here, as the http client is actually hitting the localhost
	// proxy, and the auth token will ultimately be added as necessary for whatever proxy
	// ends up getting hit.
	if bytes, err := fetchCloudConfig(client, url, ""); err == nil {
		// bytes will be nil if the config is unchanged (not modified)
		if bytes != nil {
			//log.Debugf("Downloaded config:\n %v", string(bytes))
			mutate = func(ycfg yamlconf.Config) error {
				log.Debugf("Merging cloud configuration")
				cfg := ycfg.(*Config)
				return cfg.updateFrom(bytes)
			}
		}
	} else {
		log.Errorf("Could not fetch cloud config %v", err)
		return mutate, waitTime, err
	}
	return mutate, waitTime, nil
}

// Run runs the configuration system.
func Run(updateHandler func(updated *Config)) error {
	for {
		next := m.Next()
		nextCfg := next.(*Config)
		err := updateGlobals(nextCfg)
		if err != nil {
			return err
		}
		updateHandler(nextCfg)
	}
}

func updateGlobals(cfg *Config) error {
	globals.InstanceId = cfg.InstanceId
	err := globals.SetTrustedCAs(cfg.TrustedCACerts())
	if err != nil {
		return fmt.Errorf("Unable to configure trusted CAs: %s", err)
	}
	return nil
}

// Update updates the configuration using the given mutator function.
func Update(mutate func(cfg *Config) error) error {
	return m.Update(func(ycfg yamlconf.Config) error {
		return mutate(ycfg.(*Config))
	})
}

// InConfigDir returns the path to the given filename inside of the configdir.
func InConfigDir(filename string) (string, string, error) {
	cdir := *configdir

	if cdir == "" {
		cdir = appdir.General("Lantern")
	}

	log.Debugf("Using config dir %v", cdir)
	if _, err := os.Stat(cdir); err != nil {
		if os.IsNotExist(err) {
			// Create config dir
			if err := os.MkdirAll(cdir, 0750); err != nil {
				return "", "", fmt.Errorf("Unable to create configdir at %s: %s", cdir, err)
			}
		}
	}

	return cdir, filepath.Join(cdir, filename), nil
}

// TrustedCACerts returns a slice of PEM-encoded certs for the trusted CAs
func (cfg *Config) TrustedCACerts() []string {
	certs := make([]string, 0, len(cfg.TrustedCAs))
	for _, ca := range cfg.TrustedCAs {
		certs = append(certs, ca.Cert)
	}
	return certs
}

// GetVersion implements the method from interface yamlconf.Config
func (cfg *Config) GetVersion() int {
	return cfg.Version
}

// SetVersion implements the method from interface yamlconf.Config
func (cfg *Config) SetVersion(version int) {
	cfg.Version = version
}

// ApplyDefaults implements the method from interface yamlconf.Config
//
// ApplyDefaults populates default values on a Config to make sure that we have
// a minimum viable config for running.  As new settings are added to
// flashlight, this function should be updated to provide sensible defaults for
// those settings.
func (cfg *Config) ApplyDefaults() {
	if cfg.Role == "" {
		cfg.Role = "client"
	}

	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:8787"
	}

	if cfg.UIAddr == "" {
		cfg.UIAddr = "127.0.0.1:16823"
	}

	if cfg.CloudConfig == "" {
		cfg.CloudConfig = defaultCloudConfigUrl
	}

	if cfg.InstanceId == "" {
		cfg.InstanceId = uuid.New()
	}

	// Make sure we always have a stats config
	if cfg.Stats == nil {
		cfg.Stats = &statreporter.Config{}
	}

	if cfg.Stats.StatshubAddr == "" {
		cfg.Stats.StatshubAddr = *statshubAddr
	}

	if cfg.Client != nil && cfg.Role == "client" {
		cfg.applyClientDefaults()
	}

	if cfg.ProxiedSites == nil {
		log.Debugf("Adding empty proxiedsites")
		cfg.ProxiedSites = &proxiedsites.Config{
			Delta: &proxiedsites.Delta{
				Additions: []string{},
				Deletions: []string{},
			},
			Cloud: []string{},
		}
	}

	if cfg.ProxiedSites.Cloud == nil || len(cfg.ProxiedSites.Cloud) == 0 {
		log.Debugf("Loading default cloud proxiedsites")
		cfg.ProxiedSites.Cloud = defaultProxiedSites
	}

	if cfg.TrustedCAs == nil || len(cfg.TrustedCAs) == 0 {
		cfg.TrustedCAs = defaultTrustedCAs
	}
}

func defaultRoundRobin() string {
	localeTerritory, err := jibber_jabber.DetectTerritory()
	if err != nil {
		localeTerritory = "us"
	}
	log.Debugf("Locale territory: %v", localeTerritory)
	return defaultRoundRobinForTerritory(localeTerritory)
}

// defaultDataCenter customizes the default data center depending on the user's locale.
func defaultRoundRobinForTerritory(localeTerritory string) string {
	lt := strings.ToLower(localeTerritory)
	datacenter := ""
	if lt == "cn" {
		datacenter = "jp"
	} else {
		datacenter = "nl"
	}
	log.Debugf("datacenter: %v", datacenter)
	return datacenter + ".fallbacks.getiantem.org"
}

func (cfg *Config) applyClientDefaults() {
	// Make sure we always have at least one masquerade set
	if cfg.Client.MasqueradeSets == nil {
		cfg.Client.MasqueradeSets = make(map[string][]*fronted.Masquerade)
	}
	if len(cfg.Client.MasqueradeSets) == 0 {
		cfg.Client.MasqueradeSets[cloudflare] = cloudflareMasquerades
	}

	// Make sure we always have at least one server
	if cfg.Client.FrontedServers == nil {
		cfg.Client.FrontedServers = make([]*client.FrontedServerInfo, 0)
	}
	if len(cfg.Client.FrontedServers) == 0 && len(cfg.Client.ChainedServers) == 0 {
		/*
			cfg.Client.FrontedServers = []*client.FrontedServerInfo{
				&client.FrontedServerInfo{
					Host:           defaultRoundRobin(),
					Port:           443,
					PoolSize:       0,
					MasqueradeSet:  cloudflare,
					MaxMasquerades: 20,
					QOS:            10,
					Weight:         4000,
					Trusted:        true,
				},
			}

			cfg.Client.ChainedServers = make(map[string]*client.ChainedServerInfo, len(fallbacks))
			for key, fb := range fallbacks {
				cfg.Client.ChainedServers[key] = fb
			}
		*/
	}

	if cfg.AutoReport == nil {
		cfg.AutoReport = new(bool)
		*cfg.AutoReport = true
	}

	if cfg.AutoLaunch == nil {
		cfg.AutoLaunch = new(bool)
		*cfg.AutoLaunch = true
		launcher.CreateLaunchFile(*cfg.AutoLaunch)
	}

	// Make sure all servers have a QOS and Weight configured
	for _, server := range cfg.Client.FrontedServers {
		if server.QOS == 0 {
			server.QOS = 5
		}
		if server.Weight == 0 {
			server.Weight = 100
		}
		if server.RedialAttempts == 0 {
			server.RedialAttempts = 2
		}
	}

	// Always make sure we have a map of ChainedServers
	if cfg.Client.ChainedServers == nil {
		cfg.Client.ChainedServers = make(map[string]*client.ChainedServerInfo)
	}

	// Sort servers so that they're always in a predictable order
	cfg.Client.SortServers()
}

func (cfg *Config) IsDownstream() bool {
	return cfg.Role == "client"
}

func (cfg *Config) IsUpstream() bool {
	return !cfg.IsDownstream()
}

func (cfg Config) cloudPollSleepTime() time.Duration {
	return time.Duration((CloudConfigPollInterval.Nanoseconds() / 2) + rand.Int63n(CloudConfigPollInterval.Nanoseconds()))
}

func loadBootstrapHttpClients(bs *client.BootstrapServers) []*http.Client {
	var clients []*http.Client
	for _, s := range bs.ChainedServers {
		log.Debugf("Fetching config using chained server: %v", s.Addr)
		dialer, er := s.Dialer()
		if er != nil {
			log.Errorf("Unable to configure chained server. Received error: %v", er)
			continue
		}
		clients = append(clients, &http.Client{
			Transport: &http.Transport{
				DisableKeepAlives: true,
				Dial:              dialer.Dial,
			},
		})
	}
	return clients
}

func fetchCloudConfig(client *http.Client, url, authToken string) ([]byte, error) {
	log.Debugf("Checking for cloud configuration at: %s", url)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("Unable to construct request for cloud config at %s: %s", url, err)
	}
	if lastCloudConfigETag[url] != "" {
		// Don't bother fetching if unchanged
		req.Header.Set(ifNoneMatch, lastCloudConfigETag[url])
	}

	if authToken != "" {
		req.Header.Set("X-LANTERN-AUTH-TOKEN", authToken)
	}

	// Prevents intermediate nodes (CloudFlare) from caching the content
	req.Header.Set("Cache-Control", "no-cache")

	// make sure to close the connection after reading the Body
	// this prevents the occasional EOFs errors we're seeing with
	// successive requests
	req.Close = true

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Unable to fetch cloud config at %s: %s", url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Debugf("Error closing response body: %v", err)
		}
	}()

	return readConfigResponse(url, resp)
}

func readConfigResponse(url string, resp *http.Response) ([]byte, error) {
	if resp.StatusCode == 304 {
		log.Debugf("Config unchanged in cloud")
		return nil, nil
	} else if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Unexpected response status: %d", resp.StatusCode)
	}

	lastCloudConfigETag[url] = resp.Header.Get(etag)
	gzReader, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Unable to open gzip reader: %s", err)
	}
	log.Debugf("Fetched cloud config")
	return ioutil.ReadAll(gzReader)
}

// updateFrom creates a new Config by 'merging' the given yaml into this Config.
// The masquerade sets, the collections of servers, and the trusted CAs in the
// update yaml  completely replace the ones in the original Config.
func (updated *Config) updateFrom(updateBytes []byte) error {
	// XXX: does this need a mutex, along with everyone that uses the config?
	oldFrontedServers := updated.Client.FrontedServers
	oldChainedServers := updated.Client.ChainedServers
	oldMasqueradeSets := updated.Client.MasqueradeSets
	oldTrustedCAs := updated.TrustedCAs
	updated.Client.FrontedServers = []*client.FrontedServerInfo{}
	updated.Client.ChainedServers = map[string]*client.ChainedServerInfo{}
	updated.Client.MasqueradeSets = map[string][]*fronted.Masquerade{}
	updated.TrustedCAs = []*CA{}
	err := yaml.Unmarshal(updateBytes, updated)
	if err != nil {
		updated.Client.FrontedServers = oldFrontedServers
		updated.Client.ChainedServers = oldChainedServers
		updated.Client.MasqueradeSets = oldMasqueradeSets
		updated.TrustedCAs = oldTrustedCAs
		return fmt.Errorf("Unable to unmarshal YAML for update: %s", err)
	}
	// Deduplicate global proxiedsites
	if len(updated.ProxiedSites.Cloud) > 0 {
		wlDomains := make(map[string]bool)
		for _, domain := range updated.ProxiedSites.Cloud {
			wlDomains[domain] = true
		}
		updated.ProxiedSites.Cloud = make([]string, 0, len(wlDomains))
		for domain, _ := range wlDomains {
			updated.ProxiedSites.Cloud = append(updated.ProxiedSites.Cloud, domain)
		}
		sort.Strings(updated.ProxiedSites.Cloud)
	}
	return nil
}

func trustedCACerts() []string {
	certs := make([]string, 0, len(defaultTrustedCAs))
	for _, ca := range defaultTrustedCAs {
		certs = append(certs, ca.Cert)
	}
	return certs
}
