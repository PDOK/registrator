package eureka

import (
	"log"
	"net/url"
	"github.com/gliderlabs/registrator/bridge"
	"github.com/pdok/go-eureka-client/eureka"
	"strconv"
	"fmt"
	"os"
	"time"
	"net/http"
	"io/ioutil"
	"strings"
)

func init() {
	bridge.Register(new(Factory), "eureka")
}

func isDebugEnabled() bool {
	v, found := os.LookupEnv("EUREKA_CLIENT_DEBUG")
	if (!found) {
		return false
	}
	b, err := strconv.ParseBool(v)
	if (err != nil) {
		return false
	}
	return b
}

type Factory struct{}

func (f *Factory) New(uri *url.URL) bridge.RegistryAdapter {

	eureka.SetDebugEnabled(isDebugEnabled())
	client := eureka.NewClient([]string{
		"http://" + uri.Host + uri.Path,
	})

	return &EurekaAdapter{client: client, registeredServices: make(map[string]RegisteredService)}
}

type EurekaAdapter struct {
	client             *eureka.Client
	registeredServices map[string]RegisteredService
}

type RegisteredService struct {
	ticker       *time.Ticker
	statusUrl    string
	registration *eureka.InstanceInfo
	stop         chan struct{}
}

// Ping will try to connect to eureka by attempting to retrieve the current list of applications.
func (r *EurekaAdapter) Ping() error {

	eurekaApps, err := r.client.GetApplications()
	if err != nil {
		return err
	}
	log.Println("Eureka AppsHashcode: ", eurekaApps.AppsHashcode)
	return nil
}

func instanceInformation(service *bridge.Service) *eureka.InstanceInfo {

	application := service.Name
	hostnameSplit := strings.SplitN(service.ID, ":", 2)
	hostname := hostnameSplit[0]
	ipadres := service.IP
	port := service.Port

	instanceId := fmt.Sprintf("%s:%s:%d", hostname, application, port)
	status := "UP"

	registration := eureka.NewInstanceInfo(instanceId, ipadres, application, ipadres, port, false, status) //Create a new instance to register

	if path := service.Attrs["context_path"]; path != "" {
		registration.Metadata = &eureka.MetaData{
			Map: make(map[string]string),
		}
		registration.Metadata.Map["context-path"] = path
	}
	return registration
}

func GetWithRetry(url string) (*http.Response, error) {
	timeout := time.Duration(15 * time.Second)
	client := http.Client{
		Timeout: timeout,
	}
	resp, err := client.Get(url)
	if err != nil {
		eureka.GetEurekaLogger().Errorf("First attempt failed, error in fetching url %s: %s", url, err.Error())
		return client.Get(url)
	}
	return resp, err
}

func getCurrentStatus(statusUrl string) string {
	resp, err := GetWithRetry(statusUrl)
	if err != nil {
		eureka.GetEurekaLogger().Errorf("Error in fetching status from url %s: %s", statusUrl, err.Error())
		return "DOWN"
	}
	defer resp.Body.Close()
	body, parseError := ioutil.ReadAll(resp.Body)

	if parseError != nil {
		eureka.GetEurekaLogger().Errorf("Error parsing response body from url %s body: %s", statusUrl, parseError.Error())
		return "DOWN"
	} else {
		if ( resp.StatusCode == http.StatusOK) {
			eureka.GetEurekaLogger().Debug("Service is UP")
			return "UP"
		} else {
			eureka.GetEurekaLogger().Errorf("Service is DOWN. Response from %s: %s", statusUrl, string(body))
			return "DOWN"
		}
	}
}

func checkHealth(registeredService *RegisteredService, client *eureka.Client) {
	currentStatus := registeredService.registration.Status
	newStatus := getCurrentStatus(registeredService.statusUrl)

	if (currentStatus != newStatus) {
		registeredService.registration.Status = newStatus
		client.RegisterInstance(registeredService.registration) //Send status change
	}
}

func checkHealthTick(registeredService *RegisteredService, client *eureka.Client) {
	for {
		select {
		case <-registeredService.ticker.C:
			checkHealth(registeredService, client)
		case <-registeredService.stop:
			log.Println("Stop health checking", registeredService.registration.InstanceId)
			registeredService.ticker.Stop()
			return
		}
	}
}

func getCheckInterval(service *bridge.Service) int {
	if service.Attrs["check_interval"] != "" {
		v, err := strconv.Atoi(service.Attrs["check_interval"])
		if err != nil {
			log.Println("eureka: check_interval must be valid int", err)
			return 30
		} else {
			return v
		}
	} else {
		return 30
	}
}

func skipService(service *bridge.Service) bool {
	return (service.Port == 51234 && (service.Name == "httpd" || service.Name == "ebb"))
}

func (r *EurekaAdapter) Register(service *bridge.Service) error {
	if skipService(service) {
		return nil
	}
	registration := instanceInformation(service)

	var registeredService RegisteredService
	if path := service.Attrs["check_http"]; path != "" {
		registration.Status = "STARTING"
		statusUrl := fmt.Sprintf("http://%s:%d%s", service.IP, service.Port, path)
		interval := getCheckInterval(service)
		quit := make(chan struct{})
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		registeredService = RegisteredService{registration: registration, ticker:ticker, stop:quit, statusUrl: statusUrl}
		go checkHealthTick(&registeredService, r.client)
	} else {
		registration.Status = "UP"
		registeredService = RegisteredService{registration: registration}
	}

	r.registeredServices[registration.InstanceId] = registeredService
	if status := service.Attrs["check_initial_status"]; status != "" {
		registration.Status = status
	}
	log.Println("Registering ", registration.InstanceId, "with status", registration.Status)
	return r.client.RegisterInstance(registration)
}

func (r *EurekaAdapter) Deregister(service *bridge.Service) error {
	if skipService(service) {
		return nil
	}
	registration := instanceInformation(service)
	log.Println("Deregistering ", registration.InstanceId)
	registeredService := r.registeredServices[registration.InstanceId]
	if registeredService.stop != nil {
		close(registeredService.stop)
	}
	delete(r.registeredServices, registration.InstanceId)
	return r.client.UnregisterInstance(registration)
}

func (r *EurekaAdapter) Refresh(service *bridge.Service) error {
	if skipService(service) {
		return nil
	}
	registration := instanceInformation(service)
	registeredService := r.registeredServices[registration.InstanceId]
	succeeded := r.client.SendHeartbeat(registeredService.registration)
	if !succeeded {
		return r.client.RegisterInstance(registeredService.registration)
	} else {
		return nil
	}
}

func (r *EurekaAdapter) Services() ([]*bridge.Service, error) {
	return []*bridge.Service{}, nil
}
