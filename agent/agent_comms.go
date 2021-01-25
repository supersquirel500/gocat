package agent

import (
	"errors"
	"fmt"

	"github.com/mitre/gocat/contact"
	"github.com/mitre/gocat/output"
)

type AgentCommsChannel struct {
	address string
	c2Protocol string
	c2Key string
	contactObj contact.Contact
	validated bool
}

// AgentCommsChannel methods

func AgentCommsFactory(address string, c2Protocol string, c2Key string) (*AgentCommsChannel, error) {
	newCommsChannel := &AgentCommsChannel{}
	if err := newCommsChannel.Initialize(address, c2Protocol, c2Key); err != nil {
		return nil, err
	}
	return newCommsChannel, nil
}

// Does not attempt to check C2 contact requirements
func (a *AgentCommsChannel) Initialize(address string, c2Protocol string, c2Key string) error {
	a.address = address
	a.c2Protocol = c2Protocol
	a.c2Key = c2Key
	a.validated = false

	// Get the contact object for the specified c2 protocol
	var err error
	a.contactObj, err = contact.GetContactByName(c2Protocol)
	if err != nil {
		return err
	}
	return nil
}

func (a *AgentCommsChannel) GetConfig() map[string]string {
	return map[string]string{
		"c2Name": a.c2Protocol,
		"c2Key": a.c2Key,
		"address": a.address,
	}
}

func (a *AgentCommsChannel) Validate(agentProfile map[string]interface{}) (bool, map[string]string) {
	if a.contactObj == nil {
		return false, nil
	}
	valid, modifications := a.contactObj.C2RequirementsMet(agentProfile, a.GetConfig())
	a.validated = valid
	return valid, modifications
}

func (a *AgentCommsChannel) GetKey() string {
	return a.c2Key
}

func (a *AgentCommsChannel) GetContact() contact.Contact {
	return a.contactObj
}

func (a *AgentCommsChannel) GetAddress() string {
	return a.address
}

func (a *AgentCommsChannel) GetContactName() string {
	if a.contactObj == nil {
		return ""
	}
	return a.contactObj.GetName()
}

func (a *AgentCommsChannel) GetProtocol() string {
	return a.c2Protocol
}

func (a *AgentCommsChannel) IsValidated() bool {
	return a.validated
}

func (a *AgentCommsChannel) GetIdentifier() string {
	return fmt.Sprintf("%s-%s", a.c2Protocol, a.address)
}

// Agent methods

func (a *Agent) GetCurrentContact() contact.Contact {
	return a.agentComms.GetContact()
}

func (a *Agent) getCurrentServerAddress() string {
	return a.agentComms.address
}

func (a *Agent) GetCurrentContactName() string {
	return a.agentComms.GetContactName()
}

func (a *Agent) getCurrentCommsProtocol() string {
	return a.agentComms.GetProtocol()
}

func (a *Agent) setInitialCommsChannel(server string, c2Config map[string]string) error {
	c2Protocol, ok := c2Config["c2Name"]
	if !ok {
		return errors.New("C2 config does not contain c2 protocol. Missing key: c2Name")
	}
	c2Key, ok := c2Config["c2Key"]
	if !ok {
		c2Key = ""
	}
	return a.ValidateAndSetCommsChannel(server, c2Protocol, c2Key)
}

func (a *Agent) addValidatedCommsChannel(commsChannel AgentCommsChannel) {
	if commsChannel.IsValidated() {
		a.validatedCommsChannels[commsChannel.GetIdentifier()] = commsChannel
	} else {
		output.VerbosePrint(fmt.Sprintf("[!] Cannot add invalid comms channel %s", commsChannel.GetIdentifier()))
	}
}

// Will return the AgentComms previously used for the given server and c2Protocol, or will return a new AgentComms
// that binds to that server/protocol pair.
func (a *Agent) GetCommunicationChannel(server string, c2Protocol string, c2Key string) (AgentCommsChannel, error) {
	commsChannelIdentifier := fmt.Sprintf("%s-%s", c2Protocol, server)
	commsChannel, ok := a.validatedCommsChannels[commsChannelIdentifier]
	if !ok {
		// Create new comms channel
		newChannel, err := AgentCommsFactory(server, c2Protocol, c2Key)
		if err != nil {
			return AgentCommsChannel{}, err
		}
		output.VerbosePrint(fmt.Sprintf("[*] Initialized comms channel using c2 contact %s", c2Protocol))
		return *newChannel, nil
	}
	return commsChannel, nil
}

func (a *Agent) ValidateAndSetCommsChannel(server string, c2Protocol string, c2Key string) error {
	commsChannel, err := a.GetCommunicationChannel(server, c2Protocol, c2Key)
	if err != nil {
		return err
	}
	return a.validateAndSetCommsChannelObj(commsChannel)
}

func (a *Agent) validateAndSetCommsChannelObj(commsChannel AgentCommsChannel) error {
	valid, profileModifications := commsChannel.Validate(a.GetFullProfile())
	c2Protocol := commsChannel.GetProtocol()
	output.VerbosePrint(fmt.Sprintf("[*] Attempting to validate channel %s", c2Protocol))
	if valid {
		a.setCommsChannel(commsChannel, profileModifications)
		output.VerbosePrint(fmt.Sprintf("[*] Set communication channel to %s", c2Protocol))
		return nil
	} else {
		return errors.New(fmt.Sprintf("Requirements not met for C2 channel %s for server %s", c2Protocol, commsChannel.GetAddress()))
	}
}

func (a *Agent) setCommsChannel(commsChannel AgentCommsChannel, profileModifications map[string]string) {
	a.addValidatedCommsChannel(commsChannel)
	if profileModifications != nil {
		a.modifyAgentConfiguration(profileModifications)
	}
	a.agentComms = commsChannel
	if a.localP2pReceivers != nil {
		for _, receiver := range a.localP2pReceivers {
			receiver.UpdateUpstreamComs(commsChannel.GetContact())
			receiver.UpdateUpstreamServer(commsChannel.GetAddress())
		}
	}
}

func (a *Agent) UpdateSuccessfulContacts() {
	// Only check if agent was in the process of switching contacts
	if a.tryingSwitchedContact {
		identifier := fmt.Sprintf("%s-%s", a.agentComms.GetProtocol(), a.getCurrentServerAddress())
		for _, commsChannel := range a.successfulCommsChannels {
			if commsChannel.GetIdentifier() == identifier {
				// We have already successfully used this contact before.
				return
			}
		}

		// Add comms channel to list
		a.successfulCommsChannels = append(a.successfulCommsChannels, a.agentComms)
		output.VerbosePrint(fmt.Sprintf("[*] Added comms channel to historical list of successful contacts: ", a.agentComms.GetIdentifier()))
		a.tryingSwitchedContact = false
	}
}

func (a *Agent) switchToPreviousSuccessfulCommsChannel() error {
	numSuccessfulChannels := len(a.successfulCommsChannels)
	if numSuccessfulChannels == 0 {
		return errors.New("No previous successful channels to try.")
	}
	toTry := a.successfulCommsChannels[a.successFulCommsChannelIndex]
	a.successFulCommsChannelIndex = (a.successFulCommsChannelIndex + 1) % numSuccessfulChannels
	output.VerbosePrint(fmt.Sprintf("[*] Will attempt to switch to previously successful comms channel using %s via %s", toTry.GetProtocol(), toTry.GetAddress()))
	return a.validateAndSetCommsChannelObj(toTry)
}

func (a *Agent) SwitchC2Contact(newContactName string, newKey string) error {
	// Keep same address. If new key is not specified, use same key as current comms channel
	keyToUse := a.agentComms.GetKey()
	if len(newKey) >0 {
		keyToUse = newKey
	}
	return a.ValidateAndSetCommsChannel(a.getCurrentServerAddress(), newContactName, keyToUse)
}