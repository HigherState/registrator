package bridge

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	dockerapi "github.com/fsouza/go-dockerclient"
)

var serviceIDPattern = regexp.MustCompile(`^(.+?):([a-zA-Z0-9][a-zA-Z0-9_.-]+):[0-9]+(?::udp)?$`)
var ec2internalpattern = regexp.MustCompile(`ip-\S+\.ec2\.internal`)
var consulPattern = regexp.MustCompile(`((^.+/)|^)consul(:.+)?$`)
var proxyToPortPattern = regexp.MustCompile(`-service-addr\s*=.+:(\d+)`)
var listenPortPattern = regexp.MustCompile(`-listen\s*=.*:(\d+)`)
var destinationServicePattern = regexp.MustCompile(`-service\s*=(.+)`)

type Bridge struct {
	sync.Mutex
	registry       RegistryAdapter
	docker         *dockerapi.Client
	services       map[string][]*Service
	deadContainers map[string]*DeadContainer
	config         Config
}

func New(docker *dockerapi.Client, adapterUri string, config Config) (*Bridge, error) {
	uri, err := url.Parse(adapterUri)
	if err != nil {
		return nil, errors.New("bad adapter uri: " + adapterUri)
	}
	factory, found := AdapterFactories.Lookup(uri.Scheme)
	if !found {
		return nil, errors.New("unrecognized adapter: " + adapterUri)
	}

	log.Println("Using", uri.Scheme, "adapter:", uri)
	return &Bridge{
		docker:         docker,
		config:         config,
		registry:       factory.New(uri),
		services:       make(map[string][]*Service),
		deadContainers: make(map[string]*DeadContainer),
	}, nil
}

func (b *Bridge) Ping() error {
	return b.registry.Ping()
}

func (b *Bridge) Add(containerId string) {
	b.Lock()
	defer b.Unlock()
	b.add(containerId, false)
}

func (b *Bridge) Remove(containerId string) {
	b.remove(containerId, true)
}

func (b *Bridge) RemoveOnExit(containerId string) {
	b.remove(containerId, b.shouldRemove(containerId))
}

func (b *Bridge) Refresh() {
	b.Lock()
	defer b.Unlock()

	for containerId, deadContainer := range b.deadContainers {
		deadContainer.TTL -= b.config.RefreshInterval
		if deadContainer.TTL <= 0 {
			delete(b.deadContainers, containerId)
		}
	}

	for containerId, services := range b.services {
		for _, service := range services {
			err := b.registry.Refresh(service)
			if err != nil {
				log.Println("refresh failed:", service.ID, err)
				continue
			}
			log.Println("refreshed:", containerId[:12], service.ID)
		}
	}
}

func (b *Bridge) Sync(quiet bool) {
	b.Lock()
	defer b.Unlock()

	containers, err := b.docker.ListContainers(dockerapi.ListContainersOptions{})
	if err != nil && quiet {
		log.Println("error listing containers, skipping sync")
		return
	} else if err != nil && !quiet {
		log.Fatal(err)
	}

	log.Printf("Syncing services on %d containers", len(containers))

	// NOTE: This assumes reregistering will do the right thing, i.e. nothing..
	for _, listing := range containers {
		services := b.services[listing.ID]
		if services == nil {
			b.add(listing.ID, quiet)
		} else {
			for _, service := range services {
				err := b.registry.Register(service)
				if err != nil {
					log.Println("sync register failed:", service, err)
				}
			}
		}
	}

	// Clean up services that were registered previously, but aren't
	// acknowledged within registrator
	if b.config.Cleanup {
		// Remove services if its corresponding container is not running
		log.Println("Listing non-exited containers")
		filters := map[string][]string{"status": {"created", "restarting", "running", "paused"}}
		nonExitedContainers, err := b.docker.ListContainers(dockerapi.ListContainersOptions{Filters: filters})
		if err != nil {
			log.Println("error listing nonExitedContainers, skipping sync", err)
			return
		}
		for listingId, _ := range b.services {
			found := false
			for _, container := range nonExitedContainers {
				if listingId == container.ID {
					found = true
					break
				}
			}
			// This is a container that does not exist
			if !found {
				log.Printf("stale: Removing service %s because it does not exist", listingId)
				go b.RemoveOnExit(listingId)
			}
		}

		log.Println("Cleaning up dangling services")
		extServices, err := b.registry.Services()
		if err != nil {
			log.Println("cleanup failed:", err)
			return
		}

	Outer:
		for _, extService := range extServices {
			matches := serviceIDPattern.FindStringSubmatch(extService.ID)
			if len(matches) != 3 {
				// There's no way this was registered by us, so leave it
				continue
			}
			serviceHostname := matches[1]
			if serviceHostname != Hostname {
				// ignore because registered on a different host
				continue
			}
			serviceContainerName := matches[2]
			for _, listing := range b.services {
				for _, service := range listing {
					if service.Name == extService.Name && serviceContainerName == service.Origin.container.Name[1:] {
						continue Outer
					}
				}
			}
			log.Println("dangling:", extService.ID)
			err := b.registry.Deregister(extService)
			if err != nil {
				log.Println("deregister failed:", extService.ID, err)
				continue
			}
			log.Println(extService.ID, "removed")
		}
	}
}

func (b *Bridge) add(containerId string, quiet bool) {
	if d := b.deadContainers[containerId]; d != nil {
		b.services[containerId] = d.Services
		delete(b.deadContainers, containerId)
	}

	if b.services[containerId] != nil {
		log.Println("container, ", containerId[:12], ", already exists, ignoring")
		// Alternatively, remove and readd or resubmit.
		return
	}

	container, err := b.docker.InspectContainer(containerId)
	if err != nil {
		log.Println("unable to inspect container:", containerId[:12], err)
		return
	}

	ports := make(map[string]ServicePort)

	// Extract configured host port mappings, relevant when using --net=host
	for port, _ := range container.Config.ExposedPorts {
		published := []dockerapi.PortBinding{{"0.0.0.0", port.Port()}}
		ports[string(port)] = servicePort(container, port, published)
	}

	// Extract runtime port mappings, relevant when using --net=bridge
	for port, published := range container.NetworkSettings.Ports {
		ports[string(port)] = servicePort(container, port, published)
	}

	if len(ports) == 0 && !quiet {
		log.Println("ignored:", container.ID[:12], "no published ports")
		return
	}

	servicePorts := make(map[string]ServicePort)
	for key, port := range ports {
		if b.config.Internal != true && port.HostPort == "" {
			if !quiet {
				log.Println("ignored:", container.ID[:12], "port", port.ExposedPort, "not published on host")
			}
			continue
		}
		servicePorts[key] = port
	}

	isGroup := len(servicePorts) > 1
	for _, port := range servicePorts {
		service := b.newService(port, isGroup)
		if service == nil {
			if !quiet {
				log.Println("ignored:", container.ID[:12], "service on port", port.ExposedPort)
			}
			continue
		}
		if b.config.Awsvpc && b.config.HostIp != "" && b.config.HostIp != service.IP {
			log.Println("ignored:", container.ID[:12], "service on port", port.ExposedPort, fmt.Sprintf("container awsvpc ip %s does not match registrator ip %s", service.IP, b.config.HostIp))
		}
		err := b.registry.Register(service)
		if err != nil {
			log.Println("register failed:", service, err)
			continue
		}
		b.services[container.ID] = append(b.services[container.ID], service)
		log.Println("added:", container.ID[:12], service.ID)
	}
}

func (b *Bridge) newService(port ServicePort, isgroup bool) *Service {
	container := port.container
	defaultName := strings.Split(path.Base(container.Config.Image), ":")[0]

	var hostname string
	containerHostname := container.Config.Hostname
	if b.config.Awsvpc == true && ec2internalpattern.MatchString(containerHostname) {
		hostname = containerHostname
	} else {
		// not every container may be running in a task with awsvpc networking mode
		// so fall back to the instance hostname for containers which are not
		hostname = Hostname
	}

	// not sure about this logic. kind of want to remove it.
	if hostname == "" {
		hostname = port.HostIP
	}

	if port.HostIP == "0.0.0.0" {
		ip, err := net.ResolveIPAddr("ip", hostname)
		if err == nil {
			port.HostIP = ip.String()
		}
	}

	if b.config.HostIp != "" {
		port.HostIP = b.config.HostIp
	}

	metadata, metadataFromPort := serviceMetaData(container.Config, port.ExposedPort)

	ignore := mapDefault(metadata, "ignore", "")
	if ignore != "" {
		return nil
	}

	serviceName := mapDefault(metadata, "name", "")
	if serviceName == "" {
		if b.config.Explicit {
			return nil
		}
		serviceName = defaultName
	}

	service := new(Service)
	service.Origin = port
	service.ID = hostname + ":" + container.Name[1:] + ":" + port.ExposedPort
	service.Name = serviceName
	if isgroup && !metadataFromPort["name"] {
		service.Name += "-" + port.ExposedPort
	}
	var p int

	if b.config.Internal == true {
		service.IP = port.ExposedIP
		p, _ = strconv.Atoi(port.ExposedPort)
	} else if b.config.Awsvpc && b.config.UseIpFromLabel != "" {
		// if we are in awsvpc mode, use the ip from label if it is set, otherwise resolve the hostname and use that
		containerIp := container.Config.Labels[b.config.UseIpFromLabel]
		if containerIp != "" {
			slashIndex := strings.LastIndex(containerIp, "/")
			if slashIndex > -1 {
				service.RegisterIP = containerIp[:slashIndex]
			} else {
				service.RegisterIP = containerIp
			}
			log.Println("registering container IP " + service.RegisterIP + " from label '" +
				b.config.UseIpFromLabel + "'")
		}
		ip, err := net.ResolveIPAddr("ip", hostname)
		if err == nil {
			service.IP = ip.String()
			p, _ = strconv.Atoi(port.HostPort)
		}
	} else if b.config.Awsvpc {
		ip, err := net.ResolveIPAddr("ip", hostname)
		if err == nil {
			service.IP = ip.String()
			p, _ = strconv.Atoi(port.HostPort)
		}
	} else {
		service.IP = port.HostIP
		p, _ = strconv.Atoi(port.HostPort)
	}
	service.Port = p

	// set consul connect proxy port if defined in metadata
	proxyport := mapDefault(metadata, "proxyport", "")
	if proxyport != "" {
		pp, _ := strconv.Atoi(proxyport)
		service.ProxyPort = pp
	}

	if b.config.Awsvpc == false && b.config.UseIpFromLabel != "" {
		containerIp := container.Config.Labels[b.config.UseIpFromLabel]
		if containerIp != "" {
			slashIndex := strings.LastIndex(containerIp, "/")
			if slashIndex > -1 {
				service.IP = containerIp[:slashIndex]
			} else {
				service.IP = containerIp
			}
			log.Println("using container IP " + service.IP + " from label '" +
				b.config.UseIpFromLabel + "'")
		} else {
			log.Println("Label '" + b.config.UseIpFromLabel +
				"' not found in container configuration")
		}
	}

	// NetworkMode can point to another container (kuberenetes pods)
	networkMode := container.HostConfig.NetworkMode
	if b.config.Awsvpc == false && networkMode != "" {
		if strings.HasPrefix(networkMode, "container:") {
			networkContainerId := strings.Split(networkMode, ":")[1]
			log.Println(service.Name + ": detected container NetworkMode, linked to: " + networkContainerId[:12])
			networkContainer, err := b.docker.InspectContainer(networkContainerId)
			if err != nil {
				log.Println("unable to inspect network container:", networkContainerId[:12], err)
			} else {
				service.IP = networkContainer.NetworkSettings.IPAddress
				log.Println(service.Name + ": using network container IP " + service.IP)
			}
		}
	}

	var combinedTags []string
	if port.PortType == "udp" {
		combinedTags = combineTags(
			mapDefault(metadata, "tags", ""), b.config.ForceTags, "udp")
		service.ID = service.ID + ":udp"
	} else {
		combinedTags = combineTags(
			mapDefault(metadata, "tags", ""), b.config.ForceTags)
	}

	if b.config.Awsvpc {
		combinedTags = append(combinedTags, b.config.EcsTaskArnTag+"="+container.Config.Labels["com.amazonaws.ecs.task-arn"])
	}
	service.Tags = combinedTags

	id := mapDefault(metadata, "id", "")
	if id != "" {
		service.ID = id
	}

	delete(metadata, "id")
	delete(metadata, "tags")
	delete(metadata, "name")
	delete(metadata, "proxyport")
	service.Attrs = metadata
	service.TTL = b.config.RefreshTtl

	return service
}

func (b *Bridge) remove(containerId string, deregister bool) {
	b.Lock()
	defer b.Unlock()

	if deregister {
		deregisterAll := func(services []*Service) {
			for _, service := range services {
				err := b.registry.Deregister(service)
				if err != nil {
					log.Println("deregister failed:", service.ID, err)
					continue
				}
				log.Println("removed:", containerId[:12], service.ID)
			}
		}
		deregisterAll(b.services[containerId])
		if d := b.deadContainers[containerId]; d != nil {
			deregisterAll(d.Services)
			delete(b.deadContainers, containerId)
		}
	} else if b.config.RefreshTtl != 0 && b.services[containerId] != nil {
		// need to stop the refreshing, but can't delete it yet
		b.deadContainers[containerId] = &DeadContainer{b.config.RefreshTtl, b.services[containerId]}
	}
	delete(b.services, containerId)
}

// bit set on ExitCode if it represents an exit via a signal
var dockerSignaledBit = 128

func (b *Bridge) shouldRemove(containerId string) bool {
	if b.config.DeregisterCheck == "always" {
		return true
	}
	container, err := b.docker.InspectContainer(containerId)
	if _, ok := err.(*dockerapi.NoSuchContainer); ok {
		// the container has already been removed from Docker
		// e.g. probabably run with "--rm" to remove immediately
		// so its exit code is not accessible
		log.Printf("registrator: container %v was removed, could not fetch exit code", containerId[:12])
		return true
	}

	switch {
	case err != nil:
		log.Printf("registrator: error fetching status for container %v on \"die\" event: %v\n", containerId[:12], err)
		return false
	case container.State.Running:
		log.Printf("registrator: not removing container %v, still running", containerId[:12])
		return false
	case container.State.ExitCode == 0:
		return true
	case container.State.ExitCode&dockerSignaledBit == dockerSignaledBit:
		return true
	}
	return false
}

var Hostname string

func init() {
	// It's ok for Hostname to ultimately be an empty string
	// An empty string will fall back to trying to make a best guess
	Hostname, _ = os.Hostname()
}

// Contains tells whether a contains x.
func contains(a []string, x string) bool {
	for _, n := range a {
		if x == n {
			return true
		}
	}
	return false
}

func isConnectProxy(container *dockerapi.Container) bool {
	cmd := container.Config.Cmd
	if contains(cmd, "connect") == true && contains(cmd, "proxy") == true {
		//if contains(cmd, "connect") == true && contains(cmd, "proxy") == true && consulPattern.MatchString(container.Image) == true {
		return true
	} else {
		return false
	}
}
