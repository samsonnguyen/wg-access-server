package config

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v2"

	"github.com/place1/wg-access-server/pkg/authnz/authconfig"
	"github.com/vishvananda/netlink"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"gopkg.in/alecthomas/kingpin.v2"
)

type AppConfig struct {
	LogLevel        string `yaml:"loglevel"`
	DisableMetadata bool   `yaml:"disableMetadata"`
	AdminSubject    string `yaml:"adminSubject"`
	AdminPassword   string `yaml:"adminPassword"`
	// Port sets the port that the web UI will listen on.
	// Defaults to 8000
	Port int `yaml:"port"`
	// The storage backend where device configuration will
	// be persisted.
	// Supports memory:// file:// postgres:// mysql:// sqlite3://
	// Defaults to memory://
	Storage   string `yaml:"storage"`
	WireGuard struct {
		// The network interface name of the WireGuard
		// network device.
		// Defaults to wg0
		InterfaceName string `yaml:"interfaceName"`
		// The WireGuard PrivateKey
		// If this value is lost then any existing
		// clients (WireGuard peers) will no longer
		// be able to connect.
		// Clients will either have to manually update
		// their connection configuration or setup
		// their VPN again using the web ui (easier for most people)
		PrivateKey string `yaml:"privateKey"`
		// ExternalAddress is the address that clients
		// use to connect to the wireguard interface
		// By default, this will be empty and the web ui
		// will use the current page's origin.
		ExternalHost *string `yaml:"externalHost"`
		// The WireGuard ListenPort
		// Defaults to 51820
		Port int `yaml:"port"`
	} `yaml:"wireguard"`
	VPN struct {
		// CIDR configures a network address space
		// that client (WireGuard peers) will be allocated
		// an IP address from
		// defaults to 10.44.0.0/24
		CIDR string `yaml:"cidr"`
		// GatewayInterface will be used in iptable forwarding
		// rules that send VPN traffic from clients to this interface
		// Most use-cases will want this interface to have access
		// to the outside internet
		GatewayInterface string `yaml:"gatewayInterface"`
		// The "AllowedIPs" for VPN clients.
		// This value will be included in client config
		// files and in server-side iptable rules
		// to enforce network access.
		// defaults to ["0.0.0.0/1", "128.0.0.0/1"]
		AllowedIPs []string `yaml:"AllowedIPs"`
	}
	DNS struct {
		// Enabled allows you to turn on/off
		// the VPN DNS proxy feature.
		// DNS Proxying is enabled by default.
		Enabled bool `yaml:"enabled"`
		// Upstream configures the addresses of upstream
		// DNS servers to which client DNS requests will be sent to.
		// Defaults the host's upstream DNS servers (via resolveconf)
		// or 1.1.1.1 if resolveconf cannot be used.
		// NOTE: currently wg-access-server will only use the first upstream.
		Upstream []string `yaml:"upstream"`
	} `yaml:"dns"`
	// Auth configures optional authentication backends
	// to controll access to the web ui.
	// Devices will be managed on a per-user basis if any
	// auth backends are configured.
	// If no authentication backends are configured then
	// the server will not require any authentication.
	Auth authconfig.AuthConfig `yaml:"auth"`
}

var (
	app             = kingpin.New("wg-access-server", "An all-in-one WireGuard Access Server & VPN solution")
	configPath      = app.Flag("config", "Path to a config file").Envar("CONFIG").String()
	logLevel        = app.Flag("log-level", "Log level (debug, info, error)").Envar("LOG_LEVEL").Default("info").String()
	webPort         = app.Flag("web-port", "The port that the web ui server will listen on").Envar("WEB_PORT").Default("8000").Int()
	wireguardPort   = app.Flag("wireguard-port", "The port that the Wireguard server will listen on").Envar("WIREGUARD_PORT").Default("51820").Int()
	storage         = app.Flag("storage", "The storage backend connection string").Envar("STORAGE").Default("memory://").String()
	privateKey      = app.Flag("wireguard-private-key", "Wireguard private key").Envar("WIREGUARD_PRIVATE_KEY").String()
	disableMetadata = app.Flag("disable-metadata", "Disable metadata collection (i.e. metrics)").Envar("DISABLE_METADATA").Default("false").Bool()
	adminUsername   = app.Flag("admin-username", "Admin username (defaults to admin)").Envar("ADMIN_USERNAME").String()
	adminPassword   = app.Flag("admin-password", "Admin password (provide plaintext, stored in-memory only)").Envar("ADMIN_PASSWORD").String()
	upstreamDNS     = app.Flag("upstream-dns", "An upstream DNS server to proxy DNS traffic to").Envar("UPSTREAM_DNS").String()
)

func Read() *AppConfig {
	kingpin.MustParse(app.Parse(os.Args[1:]))

	// here we're filling out the config struct
	// with values from our flags/defaults.
	config := AppConfig{}
	config.LogLevel = *logLevel
	config.Port = *webPort
	config.WireGuard.InterfaceName = "wg0"
	config.WireGuard.Port = *wireguardPort
	config.VPN.CIDR = "10.44.0.0/24"
	config.DisableMetadata = *disableMetadata
	config.WireGuard.PrivateKey = *privateKey
	config.Storage = *storage
	config.VPN.AllowedIPs = []string{"0.0.0.0/0"}
	config.DNS.Enabled = true
	config.AdminPassword = *adminPassword
	config.AdminSubject = *adminUsername

	if *upstreamDNS != "" {
		config.DNS.Upstream = []string{*upstreamDNS}
	}

	if *configPath != "" {
		if b, err := ioutil.ReadFile(*configPath); err == nil {
			if err := yaml.Unmarshal(b, &config); err != nil {
				logrus.Fatal(errors.Wrap(err, "failed to bind configuration file"))
			}
		}
	}

	level, err := logrus.ParseLevel(config.LogLevel)
	if err != nil {
		logrus.Fatal(errors.Wrap(err, "invalid log level - should be one of fatal, error, warn, info, debug, trace"))
	}

	logrus.SetLevel(level)
	logrus.SetReportCaller(true)
	logrus.SetFormatter(&logrus.TextFormatter{
		CallerPrettyfier: func(f *runtime.Frame) (string, string) {
			return "", fmt.Sprintf("%s:%d", filepath.Base(f.File), f.Line)
		},
	})

	if config.DisableMetadata {
		logrus.Info("Metadata collection has been disabled. No metrics or device connectivity information will be recorded or shown")
	}

	if config.VPN.GatewayInterface == "" {
		iface, err := defaultInterface()
		if err != nil {
			logrus.Warn(errors.Wrap(err, "failed to set default value for VPN.GatewayInterface"))
		} else {
			config.VPN.GatewayInterface = iface
		}
	}

	if config.WireGuard.PrivateKey == "" {
		logrus.Warn("no private key has been configured! using an in-memory private key that will be lost when the process exits!")
		key, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			logrus.Fatal(errors.Wrap(err, "failed to generate a server private key"))
		}
		config.WireGuard.PrivateKey = key.String()
	}

	if config.AdminPassword != "" && config.AdminSubject != "" {
		if config.Auth.Basic == nil {
			config.Auth.Basic = &authconfig.BasicAuthConfig{}
		}
		// htpasswd.AcceptBcrypt(config.AdminPassword)
		pw, err := bcrypt.GenerateFromPassword([]byte(config.AdminPassword), bcrypt.DefaultCost)
		if err != nil {
			logrus.Fatal(errors.Wrap(err, "failed to generate a bcrypt hash for the provided admin password"))
		}
		config.Auth.Basic.Users = append(config.Auth.Basic.Users, fmt.Sprintf("%s:%s", config.AdminSubject, string(pw)))
	}

	return &config
}

func defaultInterface() (string, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return "", errors.Wrap(err, "failed to list network interfaces")
	}
	for _, link := range links {
		routes, err := netlink.RouteList(link, 4)
		if err != nil {
			return "", errors.Wrapf(err, "failed to list routes for interface %s", link.Attrs().Name)
		}
		for _, route := range routes {
			if route.Dst == nil {
				return link.Attrs().Name, nil
			}
		}
	}
	return "", errors.New("could not determine the default network interface name")
}

// func randomPassword() string {
// 	letterRunes := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
// 	length := 12

// 	b := make([]rune, length)
// 	for i := range b {
// 		b[i] = letterRunes[rand.Intn(len(letterRunes))]
// 	}

// 	return string(b)
// }
