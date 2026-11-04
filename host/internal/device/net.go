package device

import (
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// NetPort is the TCP port the screen listens on.
const NetPort = 47322

// NameLen bounds the computer name sent in hello, in runes; the firmware
// keeps that many (link.h LINK_NAME_LEN) and cuts it on screen by width.
const NameLen = 20

var hostIDRE = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

// NetAddr checks a screen address, "host" or "host:port", and returns it
// with the default port filled in.
func NetAddr(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("屏幕地址不能为空")
	}
	host, port := addr, strconv.Itoa(NetPort)
	if h, p, err := net.SplitHostPort(addr); err == nil {
		host, port = h, p
	} else if strings.Contains(addr, ":") {
		return "", fmt.Errorf("屏幕地址 %q 格式不对，应为 IP 或 IP:端口", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("屏幕地址 %q 的端口不对", addr)
	}
	if host == "" || strings.ContainsAny(host, " /") {
		return "", fmt.Errorf("屏幕地址 %q 格式不对，应为 IP 或 IP:端口", addr)
	}
	return net.JoinHostPort(host, port), nil
}

// ValidHostID reports whether id can go in hello's id field.
func ValidHostID(id string) bool { return hostIDRE.MatchString(id) }

// CutName shortens a computer name to NameLen runes, as hello carries it.
func CutName(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= NameLen {
		return s
	}
	return string([]rune(s)[:NameLen])
}

// dialNet connects to the screen. Keepalives notice a screen that lost
// power; the serve loop also drops a link the screen stopped answering on.
func dialNet(addr string) (io.ReadWriteCloser, error) {
	d := net.Dialer{Timeout: 3 * time.Second, KeepAlive: 15 * time.Second}
	return d.Dial("tcp", addr)
}

// WiFiConfig is what the screen joins (the wifi message, §3.7). An empty
// SSID turns its WiFi off; an empty IP takes an address by DHCP.
type WiFiConfig struct {
	SSID string `json:"ssid"`
	Pass string `json:"pass"`
	IP   string `json:"ip,omitempty"`
	Mask string `json:"mask,omitempty"`
	GW   string `json:"gw,omitempty"`
}

// Check validates the fields and fills in the netmask a fixed IP needs.
func (w *WiFiConfig) Check() error {
	switch {
	case len(w.SSID) > 32:
		return errors.New("WiFi 名称最多 32 个字节")
	case w.SSID != "" && strings.TrimSpace(w.SSID) == "":
		return errors.New("WiFi 名称不能只有空格")
	case len(w.Pass) > 63:
		return errors.New("WiFi 密码最多 63 个字符")
	case w.Pass != "" && len(w.Pass) < 8:
		return errors.New("WiFi 密码至少 8 个字符（没有密码的 WiFi 留空）")
	}
	if w.SSID == "" {
		*w = WiFiConfig{}
		return nil
	}
	if w.IP == "" {
		w.Mask, w.GW = "", ""
		return nil
	}
	if w.Mask == "" {
		w.Mask = "255.255.255.0"
	}
	for _, f := range []struct{ name, v string }{{"固定 IP", w.IP}, {"子网掩码", w.Mask}, {"网关", w.GW}} {
		if f.v == "" && f.name == "网关" {
			continue // fine while every computer is on the screen's subnet
		}
		if ip := net.ParseIP(f.v); ip == nil || ip.To4() == nil {
			return fmt.Errorf("%s %q 不是 IPv4 地址", f.name, f.v)
		}
	}
	return nil
}

// answeredBy says whether msg is the net message that answers these
// settings (§3.7): the screen took them and is on its way. Earlier news
// about the WiFi as it was may arrive first; they name another network,
// another state or another kind of address.
func (w WiFiConfig) answeredBy(msg DeviceMsg) bool {
	if w.SSID == "" {
		return msg.State == "off"
	}
	return msg.State == "connecting" && msg.SSID == w.SSID && msg.Fixed == (w.IP != "")
}

// EncodeWiFi encodes the wifi message for a config already checked.
func EncodeWiFi(w WiFiConfig) []byte {
	line, _ := encodeLine(struct {
		T string `json:"t"`
		WiFiConfig
	}{"wifi", w})
	return line
}

// EncodeWiFiScan asks the screen for the networks in range (§3.8).
func EncodeWiFiScan() []byte {
	line, _ := encodeLine(struct {
		T string `json:"t"`
	}{"wifi_scan"})
	return line
}

// AP is a network the screen found (§4.6).
type AP struct {
	SSID string `json:"ssid"`
	RSSI int    `json:"rssi"` // dBm
	Open bool   `json:"open,omitempty"`
}

type scanResult struct {
	aps []AP
	err string
}

// NetStatus is the screen's WiFi as it last reported over USB (the net
// message, §4.5).
type NetStatus struct {
	State string `json:"state"` // off, connecting, connected, failed
	SSID  string `json:"ssid,omitempty"`
	IP    string `json:"ip,omitempty"`
	Port  int    `json:"port,omitempty"`
	Code  string `json:"code,omitempty"`
	Fixed bool   `json:"fixed,omitempty"` // a fixed IP rather than DHCP
	Err   string `json:"err,omitempty"`
	// Try is the number of the WiFi settings this go-dock sent that the
	// screen is on (ConfigureWiFi); 0 for settings from before.
	Try int `json:"try"`
}

// Addr is where go-dock reaches the screen, empty until it has an address.
func (n NetStatus) Addr() string {
	if n.IP == "" {
		return ""
	}
	port := n.Port
	if port == 0 {
		port = NetPort
	}
	if port == NetPort {
		return n.IP
	}
	return net.JoinHostPort(n.IP, strconv.Itoa(port))
}
