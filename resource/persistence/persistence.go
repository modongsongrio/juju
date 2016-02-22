// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package persistence

import (
	"github.com/juju/errors"
	"github.com/juju/loggo"
	"github.com/juju/names"
	jujutxn "github.com/juju/txn"
	"gopkg.in/mgo.v2/txn"

	"github.com/juju/juju/resource"
)

var logger = loggo.GetLogger("juju.resource.persistence")

// PersistenceBase exposes the core persistence functionality needed
// for resources.
type PersistenceBase interface {
	// One populates doc with the document corresponding to the given
	// ID. Missing documents result in errors.NotFound.
	One(collName, id string, doc interface{}) error

	// All populates docs with the list of the documents corresponding
	// to the provided query.
	All(collName string, query, docs interface{}) error

	// Run runs the transaction generated by the provided factory
	// function. It may be retried several times.
	Run(transactions jujutxn.TransactionSource) error

	// IncCharmModifiedVersionOps returns the operations necessary to increment
	// the CharmModifiedVersion field for the given service.
	IncCharmModifiedVersionOps(serviceID string) []txn.Op
}

// Persistence provides the persistence functionality for the
// Juju environment as a whole.
type Persistence struct {
	base PersistenceBase
}

// NewPersistence wraps the base in a new Persistence.
func NewPersistence(base PersistenceBase) *Persistence {
	return &Persistence{
		base: base,
	}
}

// ListResources returns the info for each non-pending resource of the
// identified service.
func (p Persistence) ListResources(serviceID string) (resource.ServiceResources, error) {
	logger.Tracef("listing all resources for service %q", serviceID)

	// TODO(ericsnow) Ensure that the service is still there?

	docs, err := p.resources(serviceID)
	if err != nil {
		return resource.ServiceResources{}, errors.Trace(err)
	}

	units := map[names.UnitTag][]resource.Resource{}

	var results resource.ServiceResources
	for _, doc := range docs {
		if doc.PendingID != "" {
			continue
		}

		res, err := doc2basicResource(doc)
		if err != nil {
			return resource.ServiceResources{}, errors.Trace(err)
		}
		if doc.UnitID == "" {
			results.Resources = append(results.Resources, res)
			continue
		}
		tag := names.NewUnitTag(doc.UnitID)
		units[tag] = append(units[tag], res)
	}
	for tag, res := range units {
		results.UnitResources = append(results.UnitResources, resource.UnitResources{
			Tag:       tag,
			Resources: res,
		})
	}
	return results, nil
}

// ListPendingResources returns the extended, model-related info for
// each pending resource of the identifies service.
func (p Persistence) ListPendingResources(serviceID string) ([]resource.Resource, error) {
	docs, err := p.resources(serviceID)
	if err != nil {
		return nil, errors.Trace(err)
	}

	var resources []resource.Resource
	for _, doc := range docs {
		if doc.PendingID == "" {
			continue
		}
		// doc.UnitID will always be empty here.

		res, err := doc2basicResource(doc)
		if err != nil {
			return nil, errors.Trace(err)
		}
		resources = append(resources, res)
	}
	return resources, nil
}

// GetResource returns the extended, model-related info for the non-pending
// resource.
func (p Persistence) GetResource(id string) (res resource.Resource, storagePath string, _ error) {
	doc, err := p.getOne(id)
	if err != nil {
		return res, "", errors.Trace(err)
	}

	stored, err := doc2resource(doc)
	if err != nil {
		return res, "", errors.Trace(err)
	}

	return stored.Resource, stored.storagePath, nil
}

// StageResource adds the resource in a separate staging area
// if the resource isn't already staged. If it is then
// errors.AlreadyExists is returned. A wrapper around the staged
// resource is returned which supports both finalizing and removing
// the staged resource.
func (p Persistence) StageResource(res resource.Resource, storagePath string) (*StagedResource, error) {
	if storagePath == "" {
		return nil, errors.Errorf("missing storage path")
	}

	if err := res.Validate(); err != nil {
		return nil, errors.Annotate(err, "bad resource")
	}

	stored := storedResource{
		Resource:    res,
		storagePath: storagePath,
	}
	staged := &StagedResource{
		base:   p.base,
		id:     res.ID,
		stored: stored,
	}
	if err := staged.stage(); err != nil {
		return nil, errors.Trace(err)
	}
	return staged, nil
}

// SetResource sets the info for the resource.
func (p Persistence) SetResource(res resource.Resource) error {
	stored, err := p.getStored(res)
	if errors.IsNotFound(err) {
		stored = storedResource{Resource: res}
	} else if err != nil {
		return errors.Trace(err)
	}
	// TODO(ericsnow) Ensure that stored.Resource matches res? If we do
	// so then the following line is unnecessary.
	stored.Resource = res

	// TODO(ericsnow) Ensure that the service is still there?

	if err := res.Validate(); err != nil {
		return errors.Annotate(err, "bad resource")
	}

	buildTxn := func(attempt int) ([]txn.Op, error) {
		// This is an "upsert".
		var ops []txn.Op
		switch attempt {
		case 0:
			ops = newInsertResourceOps(stored)
		case 1:
			ops = newUpdateResourceOps(stored)
		default:
			// Either insert or update will work so we should not get here.
			return nil, errors.New("setting the resource failed")
		}
		return ops, nil
	}
	if err := p.base.Run(buildTxn); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// SetUnitResource stores the resource info for a particular unit. The
// resource must already be set for the service.
func (p Persistence) SetUnitResource(unitID string, res resource.Resource) error {
	stored, err := p.getStored(res)
	if err != nil {
		return errors.Trace(err)
	}
	// TODO(ericsnow) Ensure that stored.Resource matches res? If we do
	// so then the following line is unnecessary.
	stored.Resource = res

	// TODO(ericsnow) Ensure that the service is still there?

	if err := res.Validate(); err != nil {
		return errors.Annotate(err, "bad resource")
	}

	buildTxn := func(attempt int) ([]txn.Op, error) {
		// This is an "upsert".
		var ops []txn.Op
		switch attempt {
		case 0:
			ops = newInsertUnitResourceOps(unitID, stored)
		case 1:
			ops = newUpdateUnitResourceOps(unitID, stored)
		default:
			// Either insert or update will work so we should not get here.
			return nil, errors.New("setting the resource failed")
		}
		return ops, nil
	}
	if err := p.base.Run(buildTxn); err != nil {
		return errors.Trace(err)
	}
	return nil
}

func (p Persistence) getStored(res resource.Resource) (storedResource, error) {
	doc, err := p.getOne(res.ID)
	if errors.IsNotFound(err) {
		err = errors.NotFoundf("resource %q", res.Name)
	}
	if err != nil {
		return storedResource{}, errors.Trace(err)
	}

	stored, err := doc2resource(doc)
	if err != nil {
		return stored, errors.Trace(err)
	}

	return stored, nil
}

// NewResolvePendingResourceOps generates mongo transaction operations
// to set the identified resource as active.
//
// Leaking mongo details (transaction ops) is a necessary evil since we
// do not have any machinery to facilitate transactions between
// different components.
func (p Persistence) NewResolvePendingResourceOps(resID, pendingID string) ([]txn.Op, error) {
	if pendingID == "" {
		return nil, errors.New("missing pending ID")
	}

	oldDoc, err := p.getOnePending(resID, pendingID)
	if errors.IsNotFound(err) {
		return nil, errors.NotFoundf("pending resource %q (%s)", resID, pendingID)
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	pending, err := doc2resource(oldDoc)
	if err != nil {
		return nil, errors.Trace(err)
	}

	exists := true
	if _, err := p.getOne(resID); errors.IsNotFound(err) {
		exists = false
	} else if err != nil {
		return nil, errors.Trace(err)
	}

	ops := newResolvePendingResourceOps(pending, exists)
	return ops, nil
}
