//TODO: only reload if rendered template changes

package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"runtime"
	"sort"
	"text/template"
	"time"

	"github.com/coreos/etcd/client"
	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"
	"golang.org/x/net/context"
)

type (
	// single running instance of haproxy
	instance struct {
		config  string
		cmd     *exec.Cmd
		haproxy *haproxy
		err     error
	}

	haproxy struct {
		service      string
		uri          string
		path         string
		etcd         client.Client
		template     *template.Template
		instanceChan chan *instance
		instances    []*instance
		reloadChan   chan struct{}
	}

	Backend struct {
		Name      string //Name. Defaults to key.
		IP        net.IP `json:"ip"`
		Port      uint16 `json:"port"`
		CheckPort uint16 `json:"check_port"` // defaults to port
		Weight    uint16 `json:"weight"`     // defaults to 50
	}

	Service struct {
		Port          uint16     `json:"port"`           // Service Port.
		Name          string     `json:"name"`           // Name. Defaults to key.
		Protocol      string     `json:"name"`           // Defaults to "tcp"
		CheckProtocol string     `json:"check_protocol"` // Defaults to Protocol
		HealthCheck   string     `json:"healthcheck"`    // Protocol specific check
		Backends      []*Backend `json:"-"`
		CheckInterval uint16     `json:"check_interval"` //defaults to 10 seconds
		CheckRise     uint16     `json:"check_rise"`     //defaults to 3
		CheckFall     uint16     `json:"check_fall"`     //defaults to 5
	}

	BackendByName []*Backend
)

func (a BackendByName) Len() int           { return len(a) }
func (a BackendByName) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a BackendByName) Less(i, j int) bool { return a[i].Name < a[j].Name }

func main() {
	viper.SetEnvPrefix("doki")
	viper.AutomaticEnv()

	var etcd, prefix, template, cmd string
	flag.StringVarP(&etcd, "etcd", "e", "http://127.0.0.1:4001", "etcd endpoint")
	flag.StringVarP(&prefix, "prefix", "p", "/doki-doki", "etcd prefix")
	flag.StringVarP(&template, "template", "t", "", "alternate haproxy config")
	flag.StringVarP(&cmd, "haproxy", "", "haproxy", "haproxy command.")

	viper.BindPFlag("etcd", flag.Lookup("etcd"))
	viper.BindPFlag("prefix", flag.Lookup("prefix"))
	viper.BindPFlag("template", flag.Lookup("template"))
	viper.BindPFlag("haproxy", flag.Lookup("haproxy"))

	flag.Parse()

	args := flag.Args()
	if len(args) != 1 {
		log.Fatal("need one and only one service")
	}

	h := &haproxy{
		service:      args[0],
		uri:          path.Join("/", viper.GetString("prefix"), args[0]),
		instanceChan: make(chan *instance),
		instances:    make([]*instance, 0, 2),
		reloadChan:   make(chan struct{}),
	}

	var err error

	h.path, err = getHaproxy()
	if err != nil {
		log.Fatalf("unable to find haproxy executable: %s", err)
	}

	h.template, err = getTemplate()
	if err != nil {
		log.Fatalf("unable to process template: %s", err)
	}

	fmt.Println(viper.GetString("etcd"))
	cfg := client.Config{
		Endpoints: []string{viper.GetString("etcd")},
		Transport: client.DefaultTransport,
	}
	h.etcd, err = client.New(cfg)
	if err != nil {
		log.Fatalf("etcd error: %s", err)
	}

	if err := h.Start(); err != nil {
		log.Fatal(err)
	}

}

func getHaproxy() (string, error) {
	haproxyPath, err := exec.LookPath(viper.GetString("haproxy"))
	if err != nil {
		return "", err
	}
	return haproxyPath, nil
}

func getTemplate() (*template.Template, error) {
	data := defaultTemplate

	fmt.Println(viper.GetString("template"))
	if file := viper.GetString("template"); file != "" {
		bytes, err := ioutil.ReadFile(file)
		if err != nil {
			return nil, err
		}
		data = string(bytes)
	}

	return template.New("haproxy").Parse(data)

}

func (h *haproxy) loadEtcdData() (*Service, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	k := client.NewKeysAPI(h.etcd)

	resp, err := k.Get(ctx, h.uri, &client.GetOptions{Recursive: true})
	if err != nil {
		return nil, err
	}

	s := &Service{
		Name:          h.service,
		Protocol:      "tcp",
		Backends:      make([]*Backend, 0),
		CheckInterval: 10,
		CheckRise:     3,
		CheckFall:     5,
	}

	if resp.Node == nil {
		return nil, fmt.Errorf("no nodes found for %s", h.uri)
	}

	if len(resp.Node.Nodes) == 0 {
		return nil, fmt.Errorf("no nodes found for %s", h.uri)
	}

	for _, n := range resp.Node.Nodes {
		_, key := path.Split(n.Key)
		if key == ".metadata" {
			if err := json.Unmarshal([]byte(n.Value), s); err != nil {
				return nil, fmt.Errorf("json error for %s: %s", n.Key, err)
			}
			continue
		}

		b := &Backend{
			Name:   key,
			Weight: 50,
		}

		if err := json.Unmarshal([]byte(n.Value), b); err != nil {
			return nil, fmt.Errorf("json error for %s: %s", n.Key, err)
		}

		s.Backends = append(s.Backends, b)
	}

	for _, b := range s.Backends {
		if b.Port == 0 {
			b.Port = s.Port
		}
		if b.CheckPort == 0 {
			b.CheckPort = b.Port
		}
	}

	if s.CheckProtocol == "" {
		s.CheckProtocol = s.Protocol
	}

	sort.Sort(BackendByName(s.Backends))
	return s, nil
}

// render template to a temp file. should write to a string so we can compare?
func (h *haproxy) renderTemplate(s *Service) (string, error) {
	tmp, err := ioutil.TempFile("", "doki-doki")
	if err != nil {
		return "", err
	}

	err = h.template.Execute(tmp, s)
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}

	return tmp.Name(), nil
}

func instanceFinalize(i *instance) {
	if i.config != "" {
		_ = os.Remove(i.config)
	}
}

func (h *haproxy) NewInstance() (*instance, error) {
	s, err := h.loadEtcdData()
	if err != nil {
		return nil, fmt.Errorf("etcd: %s", err)
	}

	i := &instance{
		haproxy: h,
	}
	i.config, err = h.renderTemplate(s)
	if err != nil {
		return nil, fmt.Errorf("template: %s", err)
	}

	runtime.SetFinalizer(i, instanceFinalize)

	// test config
	cmd := exec.Command(h.path, "-f", i.config, "-c")

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("template: %s", err)
	}

	return i, nil
}

// start. if pid != 0, then we assume it is a reload
func (i *instance) Start(pid int) error {

	if pid == 0 {
		i.cmd = exec.Command(i.haproxy.path, "-f", i.config)
	} else {
		i.cmd = exec.Command(i.haproxy.path, "-f", i.config, "-sf", fmt.Sprintf("%d", pid))
	}
	i.cmd.Stdout = os.Stdout
	i.cmd.Stderr = os.Stderr
	if err := i.cmd.Start(); err != nil {
		return err
	}
	go func() {
		i.err = i.cmd.Wait()
		fmt.Printf("instance: %s\n", i.err)
		i.haproxy.instanceChan <- i
	}()

	i.haproxy.instances = append(i.haproxy.instances, i)
	return nil
}

// tell it to reload
func (h *haproxy) Reload() {
	// we only need one reload "signal"
	select {
	case h.reloadChan <- struct{}{}:
	default:
	}
}

func (h *haproxy) doReload() {
	i, err := h.NewInstance()
	if err != nil {
		return
	}
	// we should lock??
	pid := 0
	if len(h.instances) > 0 {
		inst := h.instances[len(h.instances)]
		if !inst.cmd.ProcessState.Exited() {
			// TODO: check template checksum
			pid = inst.cmd.Process.Pid
		}
	}

	i.Start(pid)
}

// initial start of haproxy and maintainenece loops
func (h *haproxy) Start() error {
	i, err := h.NewInstance()
	if err != nil {
		return err
	}
	err = i.Start(0)
	if err != nil {
		return err
	}

	go func() {
		// we will only reload every x seconds
		for {
			<-h.reloadChan
			h.doReload()
			time.Sleep(30 * time.Second)
		}
	}()
	for i := range h.instanceChan {
		if i.err != nil {

			fmt.Printf("got an instance: %s\n", i.err)
			//record it
		}
		found := false
		j := 0
		for j = range h.instances {
			if h.instances[j] == i {
				found = true
				break
			}
		}
		if found {
			// hacky way to delete
			h.instances = append(h.instances[:j], h.instances[j+1:]...)
		}

		if len(h.instances) == 0 {
			err = i.Start(0)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
