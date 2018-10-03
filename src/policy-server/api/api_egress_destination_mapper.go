package api

import (
	"encoding/json"
	"fmt"
	"policy-server/store"

	"code.cloudfoundry.org/cf-networking-helpers/marshal"
)

type EgressDestinationMapper struct {
	Marshaler        marshal.Marshaler
	PayloadValidator egressDestinationsValidator
}

//go:generate counterfeiter -o fakes/egress_destinations_validator.go --fake-name EgressDestinationsValidator . egressDestinationsValidator
type egressDestinationsValidator interface {
	ValidateEgressDestinations([]EgressDestination) error
}

type DestinationsPayload struct {
	TotalDestinations  int                 `json:"total_destinations"`
	EgressDestinations []EgressDestination `json:"destinations"`
}

func (p *EgressDestinationMapper) AsBytes(egressDestinations []store.EgressDestination) ([]byte, error) {
	apiEgressDestinations := make([]EgressDestination, len(egressDestinations))

	for i, storeEgressDestination := range egressDestinations {
		apiEgressDestinations[i] = asApiEgressDestination(storeEgressDestination)
	}

	payload := &DestinationsPayload{
		TotalDestinations:  len(apiEgressDestinations),
		EgressDestinations: apiEgressDestinations,
	}

	bytes, err := p.Marshaler.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal json: %s", err)
	}
	return bytes, nil
}

func (p *EgressDestinationMapper) AsEgressDestinations(egressDestinations []byte) ([]store.EgressDestination, error) {
	payload := &DestinationsPayload{}
	err := json.Unmarshal(egressDestinations, payload)
	if err != nil {
		return []store.EgressDestination{}, fmt.Errorf("unmarshal json: %s", err)
	}

	err = p.PayloadValidator.ValidateEgressDestinations(payload.EgressDestinations)
	if err != nil {
		return []store.EgressDestination{}, fmt.Errorf("validate destinations: %s", err)
	}

	storeEgressDestinations := make([]store.EgressDestination, len(payload.EgressDestinations))
	for i, apiDest := range payload.EgressDestinations {
		storeEgressDestinations[i] = apiDest.asStoreEgressDestination()
	}
	return storeEgressDestinations, nil
}

func asApiEgressDestination(storeEgressDestination store.EgressDestination) EgressDestination {
	var ports []Ports

	if len(storeEgressDestination.Ports) > 0 {
		ports = []Ports{
			{
				Start: storeEgressDestination.Ports[0].Start,
				End:   storeEgressDestination.Ports[0].End,
			},
		}
	}

	firstIPRange := storeEgressDestination.IPRanges[0]

	apiEgressDestination := &EgressDestination{
		GUID:        storeEgressDestination.GUID,
		Name:        storeEgressDestination.Name,
		Description: storeEgressDestination.Description,
		Protocol:    storeEgressDestination.Protocol,
		Ports:       ports,
		IPRanges: []IPRange{{
			Start: firstIPRange.Start,
			End:   firstIPRange.End,
		}},
	}

	if storeEgressDestination.Protocol == "icmp" {
		//TODO: This should only be assigned when the store value is not nil
		apiEgressDestination.ICMPType = &storeEgressDestination.ICMPType
		//TODO: This should only be assigned when the store value is not nil
		apiEgressDestination.ICMPCode = &storeEgressDestination.ICMPCode
	}
	return *apiEgressDestination
}

func (d *EgressDestination) asStoreEgressDestination() store.EgressDestination {
	ipRanges := []store.IPRange{}
	for _, apiIPRange := range d.IPRanges {
		ipRanges = append(ipRanges, store.IPRange{
			Start: apiIPRange.Start,
			End:   apiIPRange.End,
		})
	}
	ports := []store.Ports{}

	for _, apiPorts := range d.Ports {
		ports = append(ports, store.Ports{
			Start: apiPorts.Start,
			End:   apiPorts.End,
		})
	}

	destination := store.EgressDestination{
		GUID:        d.GUID,
		Name:        d.Name,
		Description: d.Description,
		Protocol:    d.Protocol,
		Ports:       ports,
		IPRanges:    ipRanges,
	}

	if d.Protocol == "icmp" {
		if d.ICMPType == nil {
			d.ICMPType = &ICMPDefault
		}
		if d.ICMPCode == nil {
			d.ICMPCode = &ICMPDefault
		}
		destination.ICMPType = *d.ICMPType
		destination.ICMPCode = *d.ICMPCode
	}

	return destination
}
