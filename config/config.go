/***************************************************************
 *
 * Copyright (C) 2023, Pelican Project, Morgridge Institute for Research
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you
 * may not use this file except in compliance with the License.  You may
 * obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 ***************************************************************/

package config

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/sync/errgroup"
)

// Structs holding the OAuth2 state (and any other OSDF config needed)
type (
	TokenEntry struct {
		Expiration   int64  `yaml:"expiration"`
		AccessToken  string `yaml:"access_token"`
		RefreshToken string `yaml:"refresh_token,omitempty"`
	}

	PrefixEntry struct {
		// OSDF namespace prefix
		Prefix       string       `yaml:"prefix"`
		ClientID     string       `yaml:"client_id"`
		ClientSecret string       `yaml:"client_secret"`
		Tokens       []TokenEntry `yaml:"tokens,omitempty"`
	}

	OSDFConfig struct {

		// Top-level OSDF object
		OSDF struct {
			// List of OAuth2 client configurations
			OauthClient []PrefixEntry `yaml:"oauth_client,omitempty"`
		} `yaml:"OSDF"`
	}

	FederationDiscovery struct {
		DirectorEndpoint              string `json:"director_endpoint"`
		NamespaceRegistrationEndpoint string `json:"namespace_registration_endpoint"`
		JwksUri                       string `json:"jwks_uri"`
	}

	TokenOperation int

	TokenGenerationOpts struct {
		Operation TokenOperation
	}

	ServerType int // ServerType is a bit mask indicating which Pelican server(s) are running in the current process

	ContextKey string
)

const (
	CacheType ServerType = 1 << iota
	OriginType
	DirectorType
	RegistryType

	EgrpKey ContextKey = "egrp"
)

const (
	TokenWrite TokenOperation = iota
	TokenRead
	TokenSharedWrite
	TokenSharedRead
)

var (
	// Some of the unit tests probe behavior specific to OSDF vs Pelican.  Hence,
	// we need a way to override the preferred prefix.
	testingPreferredPrefix string

	//go:embed resources/defaults.yaml
	defaultsYaml string
	//go:embed resources/osdf.yaml
	osdfDefaultsYaml string

	// Potentially holds a directory to cleanup
	tempRunDir  string
	cleanupOnce sync.Once

	// Our global transports that only will get reconfigured if needed
	transport     *http.Transport
	onceTransport sync.Once

	// Global struct validator
	validate *validator.Validate

	// A variable indicating enabled Pelican servers in the current process
	enabledServers ServerType
	setServerOnce  sync.Once

	// Pelican version
	PelicanVersion string
)

func init() {
	validate = validator.New(validator.WithRequiredStructEnabled())
}

// Set sets a list of newServers to ServerType instance
func (sType *ServerType) SetList(newServers []ServerType) {
	for _, server := range newServers {
		*sType |= server
	}
}

// Enable a single server type in the bitmask
func (sType *ServerType) Set(server ServerType) ServerType {
	*sType |= server
	return *sType
}

// IsEnabled checks if a testServer is in the ServerType instance
func (sType ServerType) IsEnabled(testServer ServerType) bool {
	return sType&testServer == testServer
}

// Clear all values in a server type
func (sType *ServerType) Clear() {
	*sType = ServerType(0)
}

// setEnabledServer sets the global variable config.EnabledServers to include newServers.
// Since this function should only be called in config package, we mark it "private" to avoid
// reset value in other pacakge
//
// This will only be called once in a single process
func setEnabledServer(newServers ServerType) {
	setServerOnce.Do(func() {
		// For each process, we only want to set enabled servers once
		enabledServers.Set(newServers)
	})
}

// IsServerEnabled checks if testServer is enabled in the current process.
//
// Use this function to check which server(s) are running in the current process.
func IsServerEnabled(testServer ServerType) bool {
	return enabledServers.IsEnabled(testServer)
}

// Get a string slice of currently enabled servers, sorted by alphabetical order.
// By default, it calls String method of each enabled server.
// To get strings in lowerCase, set lowerCase = true.
func GetEnabledServerString(lowerCase bool) []string {
	servers := make([]string, 0)
	if enabledServers.IsEnabled(CacheType) {
		servers = append(servers, CacheType.String())
	}
	if enabledServers.IsEnabled(OriginType) {
		servers = append(servers, OriginType.String())
	}
	if enabledServers.IsEnabled(DirectorType) {
		servers = append(servers, DirectorType.String())
	}
	if enabledServers.IsEnabled(RegistryType) {
		servers = append(servers, RegistryType.String())
	}
	sort.Strings(servers)
	if lowerCase {
		for i, serverStr := range servers {
			servers[i] = strings.ToLower(serverStr)
		}
		return servers
	} else {
		return servers
	}
}

// Create a new, empty ServerType bitmask
func NewServerType() ServerType {
	return ServerType(0)
}

// Get the string representation of a ServerType instance. This is intended
// for getting the string form of a single ServerType contant, such as CacheType
// OriginType, etc. To get a string slice of enabled servers, use EnabledServerString()
func (sType ServerType) String() string {
	switch sType {
	case CacheType:
		return "Cache"
	case OriginType:
		return "Origin"
	case DirectorType:
		return "Director"
	case RegistryType:
		return "Registry"
	}
	return "Unknown"
}

func (sType *ServerType) SetString(name string) bool {
	switch strings.ToLower(name) {
	case "cache":
		*sType |= CacheType
		return true
	case "origin":
		*sType |= OriginType
		return true
	case "director":
		*sType |= DirectorType
		return true
	case "registry":
		*sType |= RegistryType
		return true
	}
	return false
}

// Based on the name of the current binary, determine the preferred "style"
// of behavior.  For example, a binary with the "osdf_" prefix should utilize
// the known URLs for OSDF.  For "pelican"-style commands, the user will
// need to manually configure the location of the director endpoint.
func GetPreferredPrefix() string {
	// Testing override to programmatically force different behaviors.
	if testingPreferredPrefix != "" {
		return testingPreferredPrefix
	}
	arg0 := strings.ToUpper(filepath.Base(os.Args[0]))
	underscore_idx := strings.Index(arg0, "_")
	if underscore_idx != -1 {
		return arg0[0:underscore_idx]
	}
	if strings.HasPrefix(arg0, "STASH") {
		return "STASH"
	} else if strings.HasPrefix(arg0, "OSDF") {
		return "OSDF"
	}
	return "PELICAN"
}

// Override the auto-detected preferred prefix; mostly meant for unittests.
// Returns the old preferred prefix.
func SetPreferredPrefix(newPref string) string {
	oldPref := testingPreferredPrefix
	testingPreferredPrefix = newPref
	return oldPref
}

// Get the list of valid prefixes for this binary.  Given there's been so
// many renames of the project (stash -> osdf -> pelican), we allow multiple
// prefixes when searching through environment variables.
func GetAllPrefixes() []string {
	prefixes := []string{GetPreferredPrefix()}

	if prefixes[0] == "OSDF" {
		prefixes = append(prefixes, "STASH", "PELICAN")
	} else if prefixes[0] == "STASH" {
		prefixes = append(prefixes, "OSDF", "PELICAN")
	}
	return prefixes
}

func DiscoverFederation() error {
	federationStr := param.Federation_DiscoveryUrl.GetString()
	externalUrlStr := param.Server_ExternalWebUrl.GetString()
	defer func() {
		// Set default guesses if these values are still unset.
		if param.Federation_DirectorUrl.GetString() == "" && enabledServers.IsEnabled(DirectorType) {
			viper.Set("Federation.DirectorUrl", externalUrlStr)
		}
		if param.Federation_RegistryUrl.GetString() == "" && enabledServers.IsEnabled(RegistryType) {
			viper.Set("Federation.RegistryUrl", externalUrlStr)
		}
		if param.Federation_JwkUrl.GetString() == "" && enabledServers.IsEnabled(DirectorType) {
			viper.Set("Federation.JwkUrl", externalUrlStr+"/.well-known/issuer.jwks")
		}
	}()
	if len(federationStr) == 0 {
		log.Debugln("Federation URL is unset; skipping discovery")
		return nil
	}
	if federationStr == externalUrlStr {
		log.Debugln("Current web engine hosts the federation; skipping auto-discovery of services")
		return nil
	}

	log.Debugln("Federation URL:", federationStr)
	curDirectorURL := param.Federation_DirectorUrl.GetString()
	curRegistryURL := param.Federation_RegistryUrl.GetString()
	curFederationJwkURL := param.Federation_JwkUrl.GetString()
	if len(curDirectorURL) != 0 && len(curRegistryURL) != 0 && len(curFederationJwkURL) != 0 {
		return nil
	}

	log.Debugln("Performing federation service discovery against endpoint", federationStr)
	federationUrl, err := url.Parse(federationStr)
	if err != nil {
		return errors.Wrapf(err, "Invalid federation value %s:", federationStr)
	}
	federationUrl.Scheme = "https"
	if len(federationUrl.Path) > 0 && len(federationUrl.Host) == 0 {
		federationUrl.Host = federationUrl.Path
		federationUrl.Path = ""
	}

	discoveryUrl, err := url.Parse(federationUrl.String())
	if err != nil {
		return errors.Wrap(err, "unable to parse federation discovery URL")
	}
	discoveryUrl.Path, err = url.JoinPath(federationUrl.Path, ".well-known/pelican-configuration")
	if err != nil {
		return errors.Wrap(err, "Unable to parse federation url because of invalid path")
	}

	httpClient := http.Client{
		Transport: GetTransport(),
		Timeout:   time.Second * 5,
	}
	req, err := http.NewRequest(http.MethodGet, discoveryUrl.String(), nil)
	if err != nil {
		return errors.Wrapf(err, "Failure when doing federation metadata request creation for %s", discoveryUrl)
	}
	req.Header.Set("User-Agent", "pelican/7")

	result, err := httpClient.Do(req)
	if err != nil {
		return errors.Wrapf(err, "Failure when doing federation metadata lookup to %s", discoveryUrl)
	}

	if result.Body != nil {
		defer result.Body.Close()
	}

	body, err := io.ReadAll(result.Body)
	if err != nil {
		return errors.Wrapf(err, "Failure when doing federation metadata read to %s", discoveryUrl)
	}

	if result.StatusCode != http.StatusOK {
		truncatedMessage := string(body)
		if len(body) > 1000 {
			truncatedMessage = string(body[:1000])
			truncatedMessage += " [... remainder truncated ...]"
		}
		return errors.Errorf("Federation metadata discovery failed with HTTP status %d.  Error message: %s", result.StatusCode, truncatedMessage)
	}

	metadata := FederationDiscovery{}
	err = json.Unmarshal(body, &metadata)
	if err != nil {
		return errors.Wrapf(err, "Failure when parsing federation metadata at %s", discoveryUrl)
	}
	if curDirectorURL == "" {
		log.Debugln("Federation service discovery resulted in director URL", metadata.DirectorEndpoint)
		viper.Set("Federation.DirectorUrl", metadata.DirectorEndpoint)
	}
	if curRegistryURL == "" {
		log.Debugln("Federation service discovery resulted in registry URL",
			metadata.NamespaceRegistrationEndpoint)
		viper.Set("Federation.RegistryUrl", metadata.NamespaceRegistrationEndpoint)
	}
	if curFederationJwkURL == "" {
		log.Debugln("Federation service discovery resulted in JWKS URL",
			metadata.JwksUri)
		viper.Set("Federation.JwkUrl", metadata.JwksUri)
	}

	return nil
}

// Return a struct representing the current (global) federation metadata
func GetFederation() FederationDiscovery {
	return FederationDiscovery{
		DirectorEndpoint:              param.Federation_DirectorUrl.GetString(),
		NamespaceRegistrationEndpoint: param.Federation_RegistryUrl.GetString(),
		JwksUri:                       param.Federation_JwkUrl.GetString(),
	}
}

// Set the current global federation metadata
func SetFederation(fd FederationDiscovery) {
	viper.Set("Federation.DirectorUrl", fd.DirectorEndpoint)
	viper.Set("Federation.RegistryUrl", fd.NamespaceRegistrationEndpoint)
	viper.Set("Federation.JwkUrl", fd.JwksUri)
}

// TODO: It's not clear that this function works correctly.  We should
// pass an errgroup here and ensure that the cleanup is complete before
// the main thread shuts down.
func cleanupDirOnShutdown(ctx context.Context, dir string) {
	tempRunDir = dir
	egrp, ok := ctx.Value(EgrpKey).(*errgroup.Group)
	if !ok {
		egrp = &errgroup.Group{}
	}
	egrp.Go(func() error {
		<-ctx.Done()
		err := CleanupTempResources()
		if err != nil {
			log.Infoln("Error when cleaning up temporary directories:", err)
		}
		return err
	})
}

func CleanupTempResources() (err error) {
	cleanupOnce.Do(func() {
		if tempRunDir != "" {
			err = os.RemoveAll(tempRunDir)
			tempRunDir = ""
		}
	})
	return
}

func getConfigBase() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".config", "pelican"), nil
}

func setupTransport() {
	//Getting timeouts and other information from defaults.yaml
	maxIdleConns := param.Transport_MaxIdleConns.GetInt()
	idleConnTimeout := param.Transport_IdleConnTimeout.GetDuration()
	transportTLSHandshakeTimeout := param.Transport_TLSHandshakeTimeout.GetDuration()
	expectContinueTimeout := param.Transport_ExpectContinueTimeout.GetDuration()
	responseHeaderTimeout := param.Transport_ResponseHeaderTimeout.GetDuration()

	transportDialerTimeout := param.Transport_DialerTimeout.GetDuration()
	transportKeepAlive := param.Transport_DialerKeepAlive.GetDuration()

	//Set up the transport
	transport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   transportDialerTimeout,
			KeepAlive: transportKeepAlive,
		}).DialContext,
		MaxIdleConns:          maxIdleConns,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   transportTLSHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
	if param.TLSSkipVerify.GetBool() {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	if caCert, err := LoadCertficate(param.Server_TLSCACertificateFile.GetString()); err == nil {
		systemPool, err := x509.SystemCertPool()
		if err == nil {
			systemPool.AddCert(caCert)
			// Ensure that we don't override the InsecureSkipVerify if it's present
			if transport.TLSClientConfig == nil {
				transport.TLSClientConfig = &tls.Config{RootCAs: systemPool}
			} else {
				transport.TLSClientConfig.RootCAs = systemPool
			}
		}
	}
}

func parseServerIssuerURL(sType ServerType) error {
	if param.Server_IssuerUrl.GetString() != "" {
		_, err := url.Parse(param.Server_IssuerUrl.GetString())
		if err != nil {
			return errors.Wrapf(err, "Failed to parse the Server.IssuerUrl %s loaded from config", param.Server_IssuerUrl.GetString())
		}
		return nil
	}

	if param.Server_IssuerHostname.GetString() != "" {
		if param.Server_IssuerPort.GetInt() != 0 { // Will be the default if not set
			// We assume any issuer is running https, otherwise we're crazy
			issuerUrl := url.URL{
				Scheme: "https",
				Host:   fmt.Sprintf("%s:%d", param.Server_IssuerHostname.GetString(), param.Server_IssuerPort.GetInt()),
			}
			viper.Set("Server.IssuerUrl", issuerUrl.String())
			return nil
		}
		return errors.New("If Server.IssuerHostname is configured, you must provide a valid port")
	}

	if sType.IsEnabled(OriginType) {
		// If Origin.Mode is set to anything that isn't "posix" or "", assume we're running a plugin and
		// that the origin's issuer URL actually uses the same port as OriginUI instead of XRootD. This is
		// because under that condition, keys are being served by the Pelican process instead of by XRootD
		originMode := param.Origin_Mode.GetString()
		if originMode == "" || originMode == "posix" {
			// In this case, we use the default set up by config.go, which uses the xrootd port
			issuerUrl, err := url.Parse(param.Origin_Url.GetString())
			if err != nil {
				return errors.Wrap(err, "Failed to parse the issuer URL from the default origin URL")
			}
			viper.Set("Server.IssuerUrl", issuerUrl.String())
			return nil
		} else {
			issuerUrl, err := url.Parse(param.Server_ExternalWebUrl.GetString())
			if err != nil {
				return errors.Wrap(err, "Failed to parse the issuer URL generated from Server.ExternalWebUrl")
			}
			viper.Set("Server.IssuerUrl", issuerUrl.String())
			return nil
		}
	} else {
		issuerUrlStr := param.Server_ExternalWebUrl.GetString()
		issuerUrl, err := url.Parse(issuerUrlStr)
		if err != nil {
			return errors.Wrap(err, "Failed to parse the issuer URL generated using the parsed Server.ExternalWebUrl")
		}
		viper.Set("Server.IssuerUrl", issuerUrl.String())
		return nil
	}
}

// function to get/setup the transport (only once)
func GetTransport() *http.Transport {
	onceTransport.Do(func() {
		setupTransport()
	})
	return transport
}

// Get singleton global validte method for field validation
func GetValidate() *validator.Validate {
	return validate
}

func InitConfig() {
	viper.SetConfigType("yaml")
	// 1) Set up defaults.yaml
	err := viper.MergeConfig(strings.NewReader(defaultsYaml))
	if err != nil {
		cobra.CheckErr(err)
	}
	// 2) Set up osdf.yaml (if needed)
	prefix := GetPreferredPrefix()
	loadOSDF := prefix == "OSDF"
	if os.Getenv("STASH_USE_TOPOLOGY") == "" {
		loadOSDF = loadOSDF || (prefix == "STASH")
	}
	if loadOSDF {
		err := viper.MergeConfig(strings.NewReader(osdfDefaultsYaml))
		if err != nil {
			cobra.CheckErr(err)
		}
	}
	if configFile := viper.GetString("config"); configFile != "" {
		viper.SetConfigFile(configFile)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Warningln("No home directory found for user -- will check for configuration yaml in /etc/pelican/")
		}

		// 3) Set up pelican.yaml (has higher precedence)
		viper.AddConfigPath(filepath.Join(home, ".config", "pelican"))
		viper.AddConfigPath(filepath.Join("/etc", "pelican"))
		viper.SetConfigType("yaml")
		viper.SetConfigName("pelican")
	}

	viper.SetEnvPrefix(prefix)
	viper.AutomaticEnv()
	// This line allows viper to use an env var like ORIGIN_VALUE to override the viper string "Origin.Value"
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	if err := viper.MergeInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			cobra.CheckErr(err)
		}
	}
	if param.Debug.GetBool() {
		SetLogging(log.DebugLevel)
	} else {
		logLevel := param.Logging_Level.GetString()
		level, err := log.ParseLevel(logLevel)
		cobra.CheckErr(err)
		SetLogging(level)
	}

	logLocation := param.Logging_LogLocation.GetString()
	if logLocation != "" {
		dir := filepath.Dir(logLocation)
		if dir != "" {
			if err := os.MkdirAll(dir, 0640); err != nil {
				log.Errorf("Failed to access/create specified directory. Error: %v", err)
				os.Exit(1)
			}
		}
		// Note: do not need to close the file, logrus does it for us
		f, err := os.OpenFile(logLocation, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0640)
		if err != nil {
			log.Errorf("Failed to access specified log file. Error: %v", err)
			os.Exit(1)
		}
		log.SetOutput(f)
	}

	if oldNsUrl := viper.GetString("Federation.NamespaceUrl"); oldNsUrl != "" {
		log.Warningln("Federation.NamespaceUrl is deprecated and will be removed in future release. Please migrate to use Federation.RegistryUrl instead")
		viper.SetDefault("Federation.RegistryUrl", oldNsUrl)
	}
}

func initConfigDir() error {
	configDir := viper.GetString("ConfigDir")
	if configDir == "" {
		if IsRootExecution() {
			configDir = "/etc/pelican"
		} else {
			configTmp, err := getConfigBase()
			if err != nil {
				return err
			}
			configDir = configTmp
		}
		viper.SetDefault("ConfigDir", configDir)
	}
	return nil
}

// Initialize Pelican server instance. Pass a bit mask of `currentServers` if you want to enable multiple services.
// Note not all configurations are supported: currently, if you enable both cache and origin then an error
// is thrown
func InitServer(ctx context.Context, currentServers ServerType) error {
	if err := initConfigDir(); err != nil {
		return errors.Wrap(err, "Failed to initialize the server configuration")
	}
	if currentServers.IsEnabled(OriginType) && currentServers.IsEnabled(CacheType) {
		return errors.New("A cache and origin cannot both be enabled in the same instance")
	}

	setEnabledServer(currentServers)

	xrootdPrefix := ""
	if currentServers.IsEnabled(OriginType) {
		xrootdPrefix = "origin"
	} else if currentServers.IsEnabled(CacheType) {
		xrootdPrefix = "cache"
	}
	configDir := viper.GetString("ConfigDir")
	viper.SetConfigType("yaml")
	viper.SetDefault("Server.TLSCertificate", filepath.Join(configDir, "certificates", "tls.crt"))
	viper.SetDefault("Server.TLSKey", filepath.Join(configDir, "certificates", "tls.key"))
	viper.SetDefault("Server.TLSCAKey", filepath.Join(configDir, "certificates", "tlsca.key"))
	viper.SetDefault("Server.SessionSecretFile", filepath.Join(configDir, "session-secret"))
	viper.SetDefault("Xrootd.RobotsTxtFile", filepath.Join(configDir, "robots.txt"))
	viper.SetDefault("Xrootd.ScitokensConfig", filepath.Join(configDir, "xrootd", "scitokens.cfg"))
	viper.SetDefault("Xrootd.Authfile", filepath.Join(configDir, "xrootd", "authfile"))
	viper.SetDefault("Xrootd.MacaroonsKeyFile", filepath.Join(configDir, "macaroons-secret"))
	viper.SetDefault("IssuerKey", filepath.Join(configDir, "issuer.jwk"))
	viper.SetDefault("Server.UIPasswordFile", filepath.Join(configDir, "server-web-passwd"))
	viper.SetDefault("Server.UIActivationCodeFile", filepath.Join(configDir, "server-web-activation-code"))
	viper.SetDefault("Server.SessionSecretFile", filepath.Join(configDir, "session-secret"))
	viper.SetDefault("OIDC.ClientIDFile", filepath.Join(configDir, "oidc-client-id"))
	viper.SetDefault("OIDC.ClientSecretFile", filepath.Join(configDir, "oidc-client-secret"))
	viper.SetDefault("Cache.ExportLocation", "/")
	viper.SetDefault("Registry.RequireKeyChaining", true)
	if IsRootExecution() {
		viper.SetDefault("Xrootd.RunLocation", filepath.Join("/run", "pelican", "xrootd", xrootdPrefix))
		viper.SetDefault("Cache.DataLocation", "/run/pelican/xcache")
		viper.SetDefault("Origin.Multiuser", true)
		viper.SetDefault("Director.GeoIPLocation", "/var/cache/pelican/maxmind/GeoLite2-City.mmdb")
		viper.SetDefault("Registry.DbLocation", "/var/lib/pelican/registry.sqlite")
		viper.SetDefault("Monitoring.DataLocation", "/var/lib/pelican/monitoring/data")
		viper.SetDefault("Shoveler.QueueDirectory", "/var/spool/pelican/shoveler/queue")
		viper.SetDefault("Shoveler.AMQPTokenLocation", "/etc/pelican/shoveler-token")
	} else {
		viper.SetDefault("Director.GeoIPLocation", filepath.Join(configDir, "maxmind", "GeoLite2-City.mmdb"))
		viper.SetDefault("Registry.DbLocation", filepath.Join(configDir, "ns-registry.sqlite"))
		viper.SetDefault("Monitoring.DataLocation", filepath.Join(configDir, "monitoring/data"))
		viper.SetDefault("Shoveler.QueueDirectory", filepath.Join(configDir, "shoveler/queue"))
		viper.SetDefault("Shoveler.AMQPTokenLocation", filepath.Join(configDir, "shoveler-token"))

		if userRuntimeDir := os.Getenv("XDG_RUNTIME_DIR"); userRuntimeDir != "" {
			runtimeDir := filepath.Join(userRuntimeDir, "pelican", xrootdPrefix)
			err := os.MkdirAll(runtimeDir, 0750)
			if err != nil {
				return err
			}
			viper.SetDefault("Xrootd.RunLocation", runtimeDir)
			viper.SetDefault("Cache.DataLocation", path.Join(runtimeDir, "xcache"))
		} else {
			dir, err := os.MkdirTemp("", "pelican-xrootd-*")
			if err != nil {
				return err
			}
			viper.SetDefault("Xrootd.RunLocation", filepath.Join(dir, xrootdPrefix))
			viper.SetDefault("Cache.DataLocation", path.Join(dir, "xcache"))
			cleanupDirOnShutdown(ctx, dir)
		}
		viper.SetDefault("Origin.Multiuser", false)
	}
	// Any platform-specific paths should go here
	err := InitServerOSDefaults()
	if err != nil {
		return errors.Wrapf(err, "Failure when setting up OS-specific configuration")
	}

	err = os.MkdirAll(param.Monitoring_DataLocation.GetString(), 0750)
	if err != nil {
		return errors.Wrapf(err, "Failure when creating a directory for the monitoring data")
	}

	err = os.MkdirAll(param.Shoveler_QueueDirectory.GetString(), 0750)
	if err != nil {
		return errors.Wrapf(err, "Failure when creating a directory for the shoveler on-disk queue")
	}

	hostname, err := os.Hostname()
	if err != nil {
		return err
	}
	viper.SetDefault("Server.Hostname", hostname)
	viper.SetDefault("Xrootd.Sitename", hostname)
	// For the rest of the function, use the hostname provided by the admin if
	// they have overridden the defaults.
	hostname = viper.GetString("Server.Hostname")

	if currentServers.IsEnabled(CacheType) {
		viper.Set("Xrootd.Port", param.Cache_Port.GetInt())
	}
	xrootdPort := param.Xrootd_Port.GetInt()
	if xrootdPort != 443 {
		viper.SetDefault("Origin.Url", fmt.Sprintf("https://%v:%v", param.Server_Hostname.GetString(), xrootdPort))
	} else {
		viper.SetDefault("Origin.Url", fmt.Sprintf("https://%v", param.Server_Hostname.GetString()))
	}

	webPort := param.Server_WebPort.GetInt()
	viper.SetDefault("Server.ExternalWebUrl", fmt.Sprint("https://", hostname, ":", webPort))
	externalAddressStr := param.Server_ExternalWebUrl.GetString()
	if _, err = url.Parse(externalAddressStr); err != nil {
		return errors.Wrap(err, fmt.Sprint("Invalid Server.ExternalWebUrl: ", externalAddressStr))
	}

	if currentServers.IsEnabled(DirectorType) && param.Federation_DirectorUrl.GetString() == "" {
		viper.SetDefault("Federation.DirectorUrl", viper.GetString("Server.ExternalWebUrl"))
	}

	tokenRefreshInterval := param.Monitoring_TokenRefreshInterval.GetDuration()
	tokenExpiresIn := param.Monitoring_TokenExpiresIn.GetDuration()

	if tokenExpiresIn == 0 || tokenRefreshInterval == 0 || tokenRefreshInterval > tokenExpiresIn {
		viper.Set("Monitoring.TokenRefreshInterval", time.Minute*59)
		viper.Set("Monitoring.TokenExpiresIn", time.Hour*1)
		log.Warningln("Invalid Monitoring.TokenRefreshInterval or Monitoring.TokenExpiresIn. Fallback to 59m for refresh interval and 1h for valid interval")
	}

	if currentServers.IsEnabled(DirectorType) {
		minStatRes := param.Director_MinStatResponse.GetInt()
		maxStatRes := param.Director_MaxStatResponse.GetInt()
		if minStatRes <= 0 || maxStatRes <= 0 {
			return errors.New("Invalid Director.MinStatResponse and Director.MaxStatResponse. MaxStatResponse and MinStatResponse must be positive integers")
		}
		if maxStatRes < minStatRes {
			return errors.New("Invalid Director.MinStatResponse and Director.MaxStatResponse. MaxStatResponse is less than MinStatResponse")
		}
	}

	// Unmarshal Viper config into a Go struct
	unmarshalledConfig, err := param.UnmarshalConfig()
	if err != nil || unmarshalledConfig == nil {
		return err
	}

	// Reset issuerPrivateJWK to ensure test cases can use their own temp IssuerKey
	issuerPrivateJWK.Store(nil)

	// As necessary, generate private keys, JWKS and corresponding certs

	// Note: This function will generate a private key in the location stored by the viper var "IssuerKey"
	// iff there isn't any valid private key present in that location
	_, err = GetIssuerPublicJWKS()
	if err != nil {
		return err
	}

	// Check if we have required files in place to set up TLS, or we will generate them
	err = GenerateCert()
	if err != nil {
		return err
	}

	// Generate the session secret and save it as the default value
	if err := GenerateSessionSecret(); err != nil {
		return err
	}

	// After we know we have the certs we need, call setupTransport (which uses those certs for its TLSConfig)
	setupTransport()

	// Setup CSRF middleware. To use it, you need to add this middleware to your chain
	// of http handlers by calling config.GetCSRFHandler()
	setupCSRFHandler()

	// Set up the server's issuer URL so we can access that data wherever we need to find keys and whatnot
	// This populates Server.IssuerUrl, and can be safely fetched using server_utils.GetServerIssuerURL()
	err = parseServerIssuerURL(currentServers)
	if err != nil {
		return err
	}

	return DiscoverFederation()
}

func InitClient() error {
	if err := initConfigDir(); err != nil {
		log.Warningln("No home directory found for user -- will check for configuration yaml in /etc/pelican/")
		viper.Set("ConfigDir", "/etc/pelican")
	}

	configDir := viper.GetString("ConfigDir")
	viper.SetDefault("IssuerKey", filepath.Join(configDir, "issuer.jwk"))

	upper_prefix := GetPreferredPrefix()

	viper.SetDefault("Client.StoppedTransferTimeout", 100)
	viper.SetDefault("Client.SlowTransferRampupTime", 100)
	viper.SetDefault("Client.SlowTransferWindow", 30)

	if upper_prefix == "OSDF" || upper_prefix == "STASH" {
		viper.SetDefault("Federation.TopologyNamespaceURL", "https://topology.opensciencegrid.org/osdf/namespaces")
	}

	viper.SetEnvPrefix(upper_prefix)
	viper.AutomaticEnv()

	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	err := viper.ReadInConfig()
	if err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return err
		}
		// Do not fail if the config file is missing
	}
	env_config_file := os.Getenv(upper_prefix + "_CONFIG_FILE")
	if len(env_config_file) != 0 {
		fp, err := os.Open(env_config_file)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		err = viper.ReadConfig(fp)
		if err != nil {
			return err
		}
	}

	// Handle all the grandfathered configuration parameters
	prefixes := GetAllPrefixes()
	prefixes_with_osg := append(prefixes, "OSG")
	for _, prefix := range prefixes_with_osg {
		if _, isSet := os.LookupEnv(prefix + "_DISABLE_HTTP_PROXY"); isSet {
			viper.Set("Client.DisableHttpProxy", true)
			break
		}
	}
	for _, prefix := range prefixes_with_osg {
		if _, isSet := os.LookupEnv(prefix + "_DISABLE_PROXY_FALLBACK"); isSet {
			viper.Set("Client.DisableProxyFallback", true)
			break
		}
	}
	for _, prefix := range prefixes {
		if val, isSet := os.LookupEnv(prefix + "_DIRECTOR_URL"); isSet {
			viper.Set("Federation.DirectorURL", val)
			break
		}
	}
	for _, prefix := range prefixes {
		if val, isSet := os.LookupEnv(prefix + "_NAMESPACE_URL"); isSet {
			viper.Set("Federation.RegistryUrl", val)
			break
		}
	}
	for _, prefix := range prefixes {
		if val, isSet := os.LookupEnv(prefix + "_TOPOLOGY_NAMESPACE_URL"); isSet {
			viper.Set("Federation.TopologyNamespaceURL", val)
			break
		}
	}

	// Check the environment variable STASHCP_MINIMUM_DOWNLOAD_SPEED (and all the prefix variants)
	var downloadLimit int64 = 1024 * 100
	var prefixes_with_cp []string
	for _, prefix := range prefixes {
		prefixes_with_cp = append(prefixes_with_cp, prefix+"CP")
	}
	for _, prefix := range append(prefixes, prefixes_with_cp...) {
		downloadLimitStr := os.Getenv(prefix + "_MINIMUM_DOWNLOAD_SPEED")
		if len(downloadLimitStr) == 0 {
			continue
		}
		var err error
		downloadLimit, err = strconv.ParseInt(downloadLimitStr, 10, 64)
		if err != nil {
			log.Errorf("Environment variable %s_MINIMUM_DOWNLOAD_SPEED=%s is not parsable as integer: %s",
				prefixes, downloadLimitStr, err.Error())
		}
		break
	}
	if viper.IsSet("MinimumDownloadSpeed") {
		viper.SetDefault("Client.MinimumDownloadSpeed", param.MinimumDownloadSpeed.GetInt())
	} else {
		viper.Set("Client.MinimumDownloadSpeed", downloadLimit)
	}

	// Handle more legacy config options
	if viper.IsSet("DisableProxyFallback") {
		viper.SetDefault("Client.DisableProxyFallback", param.DisableProxyFallback.GetBool())
	}
	if viper.IsSet("DisableHttpProxy") {
		viper.SetDefault("Client.DisableHttpProxy", param.DisableHttpProxy.GetBool())
	}

	setupTransport()

	// Unmarshal Viper config into a Go struct
	unmarshalledConfig, err := param.UnmarshalConfig()
	if err != nil || unmarshalledConfig == nil {
		return err
	}

	return DiscoverFederation()
}

func SetLogging(logLevel log.Level) {
	textFormatter := log.TextFormatter{}
	textFormatter.DisableLevelTruncation = true
	textFormatter.FullTimestamp = true
	log.SetFormatter(&textFormatter)
	log.SetLevel(logLevel)
}
