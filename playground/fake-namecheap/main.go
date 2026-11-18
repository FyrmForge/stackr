// fake-namecheap is a tiny stand-in for the Namecheap XML API, enough for
// stackr's Namecheap connector: domains.getList, domains.dns.getHosts,
// domains.dns.setHosts and domains.dns.setDefault. Zones live in memory,
// persist as JSON and are rendered to a dnsmasq config after every write.
package main

import (
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type host struct {
	Name, Type, Address, MXPref, TTL string
}

type zone struct {
	EmailType string
	Hosts     []host
}

type server struct {
	mu        sync.Mutex
	dir       string
	reloadCmd string
	zones     map[string]*zone
}

func main() {
	listen := flag.String("listen", "203.0.113.1:8080", "listen address")
	state := flag.String("state", "/var/lib/playground/namecheap", "state directory")
	zones := flag.String("zones", "stackr.test", "comma list of domains that exist")
	reload := flag.String("reload-cmd", "systemctl restart dnsmasq", "run after each write, empty disables")
	flag.Parse()
	s, err := newServer(*state, *reload, strings.Split(*zones, ","))
	if err != nil {
		log.Fatal(err)
	}
	if err := s.write(); err != nil { // dnsmasq.conf exists from the start
		log.Fatal(err)
	}
	http.HandleFunc("/xml.response", s.handle)
	log.Fatal(http.ListenAndServe(*listen, nil))
}

func newServer(dir, reload string, names []string) (*server, error) {
	s := &server{dir: dir, reloadCmd: reload, zones: map[string]*zone{}}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(filepath.Join(dir, "zones.json")); err == nil {
		if err := json.Unmarshal(b, &s.zones); err != nil {
			return nil, err
		}
	}
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" && s.zones[n] == nil {
			s.zones[n] = &zone{EmailType: "MX", Hosts: []host{{Name: "@", Type: "A", Address: "203.0.113.1", TTL: "1800"}}}
		}
	}
	return s, nil
}

func xmlErr(number, text string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(text))
	return `<ApiResponse Status="ERROR"><Errors><Error Number="` + number + `">` + b.String() + `</Error></Errors><CommandResponse/></ApiResponse>`
}

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	out := s.answer(r)
	fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?>`+out)
}

func (s *server) answer(r *http.Request) string {
	f := r.FormValue
	switch {
	case f("ApiUser") == "":
		return xmlErr("1010101", "Parameter APIUser is missing")
	case f("ApiKey") == "":
		return xmlErr("1011102", "Parameter APIKey is missing")
	case f("ClientIp") == "":
		return xmlErr("1011150", "Parameter ClientIp is missing")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cmd := strings.ToLower(f("Command"))
	ok := func(body string) string {
		return `<ApiResponse Status="OK"><Errors/><CommandResponse Type="` + esc(f("Command")) + `">` + body + `</CommandResponse></ApiResponse>`
	}
	if cmd == "namecheap.domains.getlist" {
		names := make([]string, 0, len(s.zones))
		for n := range s.zones {
			names = append(names, n)
		}
		sort.Strings(names)
		var b strings.Builder
		b.WriteString("<DomainGetListResult>")
		if p, _ := strconv.Atoi(f("Page")); p <= 1 {
			for _, n := range names {
				b.WriteString(`<Domain Name="` + esc(n) + `" IsOurDNS="true"/>`)
			}
		}
		b.WriteString(`</DomainGetListResult><Paging><TotalItems>` + strconv.Itoa(len(names)) + `</TotalItems></Paging>`)
		return ok(b.String())
	}
	domain := strings.ToLower(f("SLD") + "." + f("TLD"))
	z := s.zones[domain]
	if z == nil {
		return xmlErr("2019166", "Domain not found")
	}
	switch cmd {
	case "namecheap.domains.dns.setdefault":
		return ok(`<DomainDNSSetDefaultResult Domain="` + esc(domain) + `" Updated="true"/>`)
	case "namecheap.domains.dns.gethosts":
		var b strings.Builder
		b.WriteString(`<DomainDNSGetHostsResult Domain="` + esc(domain) + `" IsUsingOurDNS="true" EmailType="` + esc(z.EmailType) + `">`)
		for i, h := range z.Hosts {
			fmt.Fprintf(&b, `<host HostId="%d" Name="%s" Type="%s" Address="%s" MXPref="%s" TTL="%s" IsActive="true"/>`,
				i+1, esc(h.Name), esc(h.Type), esc(h.Address), esc(h.MXPref), esc(h.TTL))
		}
		b.WriteString("</DomainDNSGetHostsResult>")
		return ok(b.String())
	case "namecheap.domains.dns.sethosts":
		var hs []host
		for i := 1; f("HostName"+strconv.Itoa(i)) != ""; i++ {
			k := strconv.Itoa(i)
			h := host{Name: f("HostName" + k), Type: strings.ToUpper(f("RecordType" + k)), Address: f("Address" + k), MXPref: f("MXPref" + k), TTL: f("TTL" + k)}
			if h.Type == "CAA" && f("Tag"+k) != "" { // read back as "0 issue ca.example"
				h.Address = f("Flag"+k) + " " + f("Tag"+k) + " " + h.Address
			}
			hs = append(hs, h)
		}
		if len(hs) == 0 {
			return xmlErr("2050166", "No host records given")
		}
		z.Hosts = hs
		if e := f("EmailType"); e != "" {
			z.EmailType = e
		}
		if err := s.write(); err != nil {
			log.Print(err)
			return xmlErr("5050900", "Unknown exceptions")
		}
		return ok(`<DomainDNSSetHostsResult Domain="` + esc(domain) + `" IsSuccess="true"/>`)
	}
	return xmlErr("1010104", "Command is invalid")
}

// write persists the zones, renders dnsmasq.conf and reloads dnsmasq.
func (s *server) write() error {
	b, err := json.MarshalIndent(s.zones, "", " ")
	if err != nil {
		return err
	}
	if err := atomic(filepath.Join(s.dir, "zones.json"), b); err != nil {
		return err
	}
	if err := atomic(filepath.Join(s.dir, "dnsmasq.conf"), []byte(render(s.zones))); err != nil {
		return err
	}
	if s.reloadCmd == "" {
		return nil
	}
	f := strings.Fields(s.reloadCmd)
	if out, err := exec.Command(f[0], f[1:]...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v: %s", s.reloadCmd, err, out)
	}
	return nil
}

func atomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// render is every zone in dnsmasq syntax. A wildcard A record becomes
// address=/x/ip (dnsmasq's wildcard); exact names use host-record.
func render(zones map[string]*zone) string {
	names := make([]string, 0, len(zones))
	for n := range zones {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, d := range names {
		for _, h := range zones[d].Hosts {
			fq := fqdn(h.Name, d)
			addr := strings.TrimSuffix(h.Address, ".")
			switch h.Type {
			case "A", "AAAA":
				if rest, ok := strings.CutPrefix(fq, "*."); ok {
					fmt.Fprintf(&b, "address=/%s/%s\n", rest, h.Address)
				} else {
					fmt.Fprintf(&b, "host-record=%s,%s\n", fq, h.Address)
				}
			case "CNAME":
				fmt.Fprintf(&b, "cname=%s,%s\n", fq, addr)
			case "TXT":
				fmt.Fprintf(&b, "txt-record=%s,\"%s\"\n", fq, strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(h.Address))
			case "MX":
				pref := h.MXPref
				if pref == "" {
					pref = "10"
				}
				fmt.Fprintf(&b, "mx-host=%s,%s,%s\n", fq, addr, pref)
			case "CAA":
				f := strings.SplitN(h.Address, " ", 3)
				if len(f) != 3 {
					continue
				}
				flags, _ := strconv.Atoi(f[0])
				val := strings.Trim(f[2], `"`)
				wire := append([]byte{byte(flags), byte(len(f[1]))}, f[1]+val...)
				fmt.Fprintf(&b, "dns-rr=%s,257,%s\n", fq, hex.EncodeToString(wire))
			}
		}
	}
	return b.String()
}

func fqdn(name, domain string) string {
	switch name {
	case "", "@":
		return domain
	}
	return strings.ToLower(name) + "." + domain
}
