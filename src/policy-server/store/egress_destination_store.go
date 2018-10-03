package store

import (
	"fmt"

	"code.cloudfoundry.org/cf-networking-helpers/db"
	"github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
)

//go:generate counterfeiter -o fakes/egress_destination_repo.go --fake-name EgressDestinationRepo . egressDestinationRepo
type egressDestinationRepo interface {
	All(tx db.Transaction) ([]EgressDestination, error)
	CreateIPRange(tx db.Transaction, destinationTerminalGUID, startIP, endIP, protocol string, startPort, endPort, icmpType, icmpCode int64) (int64, error)
	UpdateIPRange(tx db.Transaction, destinationTerminalGUID, startIP, endIP, protocol string, startPort, endPort, icmpType, icmpCode int64) error
	GetByGUID(tx db.Transaction, guid ...string) ([]EgressDestination, error)
	Delete(tx db.Transaction, guid string) error
}

//go:generate counterfeiter -o fakes/destination_metadata_repo.go --fake-name DestinationMetadataRepo . destinationMetadataRepo
type destinationMetadataRepo interface {
	Create(tx db.Transaction, terminalGUID, name, description string) (int64, error)
	Delete(tx db.Transaction, terminalGUID string) error
	Update(tx db.Transaction, terminalGUID, name, description string) error
}

type EgressDestinationStore struct {
	Conn                    Database
	EgressDestinationRepo   egressDestinationRepo
	TerminalsRepo           terminalsRepo
	DestinationMetadataRepo destinationMetadataRepo
}

func (e *EgressDestinationStore) GetByGUID(guid ...string) ([]EgressDestination, error) {
	tx, err := e.Conn.Beginx()
	if err != nil {
		return []EgressDestination{}, fmt.Errorf("egress destination store create transaction: %s", err)
	}
	defer tx.Rollback()
	return e.EgressDestinationRepo.GetByGUID(tx, guid...)
}

func (e *EgressDestinationStore) All() ([]EgressDestination, error) {
	tx, err := e.Conn.Beginx()
	if err != nil {
		return []EgressDestination{}, fmt.Errorf("egress destination store create transaction: %s", err)
	}
	defer tx.Rollback()
	return e.EgressDestinationRepo.All(tx)
}

func (e *EgressDestinationStore) Delete(guid string) (EgressDestination, error) {
	tx, err := e.Conn.Beginx()
	if err != nil {
		return EgressDestination{}, fmt.Errorf("egress destination store delete transaction: %s", err)
	}

	destinations, err := e.EgressDestinationRepo.GetByGUID(tx, guid)
	if err != nil {
		tx.Rollback()
		return EgressDestination{}, fmt.Errorf("egress destination store get destination by guid: %s", err)
	}

	err = e.EgressDestinationRepo.Delete(tx, guid)
	if err != nil {
		tx.Rollback()
		return EgressDestination{}, fmt.Errorf("egress destination store delete destination: %s", err)
	}

	err = e.DestinationMetadataRepo.Delete(tx, guid)
	if err != nil {
		tx.Rollback()
		return EgressDestination{}, fmt.Errorf("egress destination store delete destination metadata: %s", err)
	}

	err = e.TerminalsRepo.Delete(tx, guid)
	if err != nil {
		tx.Rollback()
		if isForeignKeyError(err) {
			return EgressDestination{}, NewForeignKeyError(err)
		}
		return EgressDestination{}, fmt.Errorf("egress destination store delete destination terminal: %s", err)
	}

	err = tx.Commit()
	if err != nil {
		tx.Rollback()
		return EgressDestination{}, fmt.Errorf("egress destination store delete destination commit: %s", err)
	}

	if len(destinations) > 0 {
		return destinations[0], nil
	}

	return EgressDestination{}, nil
}

func (e *EgressDestinationStore) Update(egressDestinations []EgressDestination) ([]EgressDestination, error) {
	tx, err := e.Conn.Beginx()
	if err != nil {
		return nil, fmt.Errorf("egress destination store update transaction: %s", err)
	}

	//TODO ensure all of the destinations exist i.e. GetByGUIDs returns a slice of len egressDestinations

	for _, egressDestination := range egressDestinations {
		//TODO ensure the metadata exists, legacy destinations did not have them
		err := e.DestinationMetadataRepo.Update(tx, egressDestination.GUID, egressDestination.Name, egressDestination.Description)
		if err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("egress destination store update metadata: %s", err)
		}

		var startPort, endPort int64
		if len(egressDestination.Ports) > 0 {
			startPort = int64(egressDestination.Ports[0].Start)
			endPort = int64(egressDestination.Ports[0].End)
		}

		err = e.EgressDestinationRepo.UpdateIPRange(
			tx,
			egressDestination.GUID,
			egressDestination.IPRanges[0].Start,
			egressDestination.IPRanges[0].End,
			egressDestination.Protocol,
			startPort,
			endPort,
			int64(egressDestination.ICMPType),
			int64(egressDestination.ICMPCode),
		)
		if err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("egress destination store update iprange: %s", err)
		}
	}

	err = tx.Commit()

	if err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("egress destination store update commit transaction: %s", err)
	}
	return egressDestinations, nil
}

func (e *EgressDestinationStore) Create(egressDestinations []EgressDestination) ([]EgressDestination, error) {
	tx, err := e.Conn.Beginx()
	if err != nil {
		return []EgressDestination{}, fmt.Errorf("egress destination store create transaction: %s", err)
	}

	results := []EgressDestination{}
	for _, egressDestination := range egressDestinations {
		destinationTerminalGUID, err := e.TerminalsRepo.Create(tx)
		if err != nil {
			tx.Rollback()
			return []EgressDestination{}, fmt.Errorf("egress destination store create terminal: %s", err)
		}

		_, err = e.DestinationMetadataRepo.Create(tx, destinationTerminalGUID, egressDestination.Name, egressDestination.Description)
		if err != nil {
			tx.Rollback()
			if isDuplicateError(err, egressDestination.Name) {
				return []EgressDestination{}, fmt.Errorf("egress destination store create destination metadata: duplicate name error: entry with name '%s' already exists", egressDestination.Name)
			}
			return []EgressDestination{}, fmt.Errorf("egress destination store create destination metadata: %s", err)
		}

		var startPort, endPort int64
		if len(egressDestination.Ports) > 0 {
			startPort = int64(egressDestination.Ports[0].Start)
			endPort = int64(egressDestination.Ports[0].End)
		}

		_, err = e.EgressDestinationRepo.CreateIPRange(
			tx,
			destinationTerminalGUID,
			egressDestination.IPRanges[0].Start,
			egressDestination.IPRanges[0].End,
			egressDestination.Protocol,
			startPort,
			endPort,
			int64(egressDestination.ICMPType),
			int64(egressDestination.ICMPCode),
		)
		if err != nil {
			tx.Rollback()
			return []EgressDestination{}, fmt.Errorf("egress destination store create ip range: %s", err)
		}

		egressDestination.GUID = destinationTerminalGUID
		results = append(results, egressDestination)
	}

	err = tx.Commit()
	if err != nil {
		tx.Rollback()
		return []EgressDestination{}, fmt.Errorf("egress destination store commit transaction: %s", err)
	}

	return results, nil
}

func isDuplicateError(err error, name string) bool {
	switch typedErr := err.(type) {
	case *pq.Error:
		if typedErr.Code == "23505" {
			return true
		}
	case *mysql.MySQLError:
		if typedErr.Number == 1062 {
			return true
		}
	}
	return false
}

func isForeignKeyError(err error) bool {
	switch typedErr := err.(type) {
	case *pq.Error:
		if typedErr.Code == "23503" {
			return true
		}
	case *mysql.MySQLError:
		if typedErr.Number == 1451 {
			return true
		}
	}
	return false
}
