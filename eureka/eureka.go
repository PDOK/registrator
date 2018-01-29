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

func getEnvironmentVariable(key string, defaulValue string) string {
	v, found := os.LookupEnv(key)
	if !found {
		log.Println("Missing environment variable:", key)
		return defaulValue
	}
	return v
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

	return &EurekaAdapter{client: client, registeredServices: make(map[string]RegisteredService), knownApplications: make(map[string]eureka.Application)}
}

type EurekaAdapter struct {
	client             *eureka.Client
	registeredServices map[string]RegisteredService
	knownApplications  map[string]eureka.Application
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

	//Store current situation as known in Eureka for a while
	for _, application := range eurekaApps.Applications {
		r.knownApplications[application.Name] = application
	}
	log.Println("Already registered number of applications: " , len(r.knownApplications))

	//Clear initial situation after 90 seconds, first refresh comes at 60 seconds (ttl)
	timer := time.NewTimer(90 * time.Second)
	go func() {
		<- timer.C
		for k := range r.knownApplications {
			delete(r.knownApplications, k)
		}
		log.Println("Cleared the known applications")
	}()

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
		createMetadataMap(registration)
		registration.Metadata.Map["context-path"] = path
	}
	if path := service.Attrs["depends_on"]; path != "" {
		createMetadataMap(registration)
		registration.Metadata.Map["depends_on"] = path
	}
	return registration
}
func createMetadataMap(registration *eureka.InstanceInfo) {
	if registration.Metadata == nil {
		registration.Metadata = &eureka.MetaData{
			Map: make(map[string]string),
		}
	}
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
	return service.Port == 51234 && (service.Name == getEnvironmentVariable("PLP_HTTPD_SERVICE_NAME", "yoda-httpd"))
}

func (r *EurekaAdapter) Register(service *bridge.Service) error {
	if skipService(service) {
		return nil
	}
	registration := instanceInformation(service)

	var registeredService RegisteredService
	if path := service.Attrs["check_http"]; path != "" {
		statusUrl := fmt.Sprintf("http://%s:%d%s", service.IP, service.Port, path)

		// In case of a restart/redeploy the container is probably already running for a long time, we should not mark it as STARTING
		// In case of docker daemon all containers get restarted, so status might be wrong
		oldRegistration, exactMatch := r.findOldRegistration(registration)
		if oldRegistration != nil {
			if !exactMatch {
				//Potential ghost container, same application, same machine different port found after restart
				_, existing := r.registeredServices[oldRegistration.InstanceId]
				if !existing {
					//Remove from Eureka
					log.Println("Ghost container found with instanceId: " , oldRegistration.InstanceId)
					r.client.UnregisterInstance(oldRegistration)
					log.Println("Ghost container removed with instanceId: " , oldRegistration.InstanceId)
				}
				// Only a Ghost container found, so this new container is just starting
				// Make the new container as starting
				registration.Status = "STARTING"
			} else {
				// Same container (with same instanceID) is already registered, we directly check the real status
				log.Println("Container with instanceId: ", oldRegistration.InstanceId , "found, directly checking its status")
				registration.Status = getCurrentStatus(statusUrl)
				log.Println("Container with instanceId: ", oldRegistration.InstanceId, " will get status: ", registration.Status)
			}
		} else {
			// Mark container as starting
			registration.Status = "STARTING"
		}

		interval := getCheckInterval(service)
		quit := make(chan struct{})
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		registeredService = RegisteredService{registration: registration, ticker: ticker, stop: quit, statusUrl: statusUrl}
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

func (r *EurekaAdapter) findOldRegistration(registration *eureka.InstanceInfo) (*eureka.InstanceInfo, bool) {
	application, found := r.knownApplications[registration.App]
	if found {
		for _, instance := range application.Instances {
			if instance.InstanceId == registration.InstanceId {
				return &instance, true
			}

			if instance.IpAddr == registration.IpAddr {
				return &instance, instance.Port == registration.Port
			}
		}
	}
	return nil, false
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
