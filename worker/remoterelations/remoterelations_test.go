// Copyright 2016 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package remoterelations_test

import (
	"reflect"

	"github.com/juju/errors"
	jujutesting "github.com/juju/testing"
	jc "github.com/juju/testing/checkers"
	gc "gopkg.in/check.v1"
	"gopkg.in/juju/names.v2"
	"gopkg.in/juju/worker.v1"
	"gopkg.in/macaroon.v1"

	"github.com/juju/juju/api"
	"github.com/juju/juju/apiserver/common"
	"github.com/juju/juju/apiserver/params"
	apiservertesting "github.com/juju/juju/apiserver/testing"
	"github.com/juju/juju/core/life"
	coretesting "github.com/juju/juju/testing"
	"github.com/juju/juju/watcher"
	"github.com/juju/juju/worker/remoterelations"
	"github.com/juju/juju/worker/workertest"
)

var _ = gc.Suite(&remoteRelationsSuite{})

type remoteRelationsSuite struct {
	coretesting.BaseSuite

	resources             *common.Resources
	authorizer            *apiservertesting.FakeAuthorizer
	relationsFacade       *mockRelationsFacade
	remoteRelationsFacade *mockRemoteRelationsFacade
	config                remoterelations.Config
	stub                  *jujutesting.Stub
}

func (s *remoteRelationsSuite) SetUpTest(c *gc.C) {
	s.BaseSuite.SetUpTest(c)

	s.stub = new(jujutesting.Stub)
	s.relationsFacade = newMockRelationsFacade(s.stub)
	s.remoteRelationsFacade = newMockRemoteRelationsFacade(s.stub)
	s.config = remoterelations.Config{
		ModelUUID:       "local-model-uuid",
		RelationsFacade: s.relationsFacade,
		NewRemoteModelFacadeFunc: func(*api.Info) (remoterelations.RemoteModelRelationsFacadeCloser, error) {
			return s.remoteRelationsFacade, nil
		},
	}
}

func (s *remoteRelationsSuite) waitForWorkerStubCalls(c *gc.C, expected []jujutesting.StubCall) {
	waitForStubCalls(c, s.stub, expected)
}

func waitForStubCalls(c *gc.C, stub *jujutesting.Stub, expected []jujutesting.StubCall) {
	var calls []jujutesting.StubCall
	for a := coretesting.LongAttempt.Start(); a.Next(); {
		calls = stub.Calls()
		if reflect.DeepEqual(calls, expected) {
			return
		}
	}
	c.Fatalf("failed to see expected calls. saw: %v", calls)
}

func (s *remoteRelationsSuite) assertRemoteApplicationWorkers(c *gc.C) worker.Worker {
	// Checks that the main worker loop responds to remote application events
	// by starting relevant relation watchers.
	s.relationsFacade.remoteApplications["db2"] = newMockRemoteApplication("db2", "db2url")
	s.relationsFacade.remoteApplications["mysql"] = newMockRemoteApplication("mysql", "mysqlurl")
	applicationNames := []string{"db2", "mysql"}
	s.relationsFacade.remoteApplicationsWatcher.changes <- applicationNames

	w, err := remoterelations.New(s.config)
	c.Assert(err, jc.ErrorIsNil)
	expected := []jujutesting.StubCall{
		{"WatchRemoteApplications", nil},
		{"RemoteApplications", []interface{}{[]string{"db2", "mysql"}}},
		{"WatchRemoteApplicationRelations", []interface{}{"db2"}},
		{"WatchRemoteApplicationRelations", []interface{}{"mysql"}},
	}
	s.waitForWorkerStubCalls(c, expected)
	for _, app := range applicationNames {
		w, ok := s.relationsFacade.remoteApplicationRelationsWatcher(app)
		c.Check(ok, jc.IsTrue)
		waitForStubCalls(c, &w.Stub, []jujutesting.StubCall{
			{"Changes", nil},
		})
	}
	return w
}

func (s *remoteRelationsSuite) TestRemoteApplicationWorkers(c *gc.C) {
	w := s.assertRemoteApplicationWorkers(c)
	workertest.CleanKill(c, w)

	// Check that relation watchers are stopped with the worker.
	applicationNames := []string{"db2", "mysql"}
	for _, app := range applicationNames {
		w, ok := s.relationsFacade.remoteApplicationRelationsWatcher(app)
		c.Check(ok, jc.IsTrue)
		c.Check(w.killed(), jc.IsTrue)
	}
}

func (s *remoteRelationsSuite) TestRemoteApplicationRemoved(c *gc.C) {
	// Checks that when a remote application is removed, the relation
	// worker is killed.
	w := s.assertRemoteApplicationWorkers(c)
	defer workertest.CleanKill(c, w)
	s.stub.ResetCalls()

	relWatcher, _ := s.relationsFacade.removeApplication("mysql")
	s.relationsFacade.remoteApplicationsWatcher.changes <- []string{"mysql"}
	for a := coretesting.LongAttempt.Start(); a.Next(); {
		_, ok := s.relationsFacade.remoteApplicationRelationsWatcher("mysql")
		if !ok {
			break
		}
	}
	c.Check(relWatcher.killed(), jc.IsTrue)
	expected := []jujutesting.StubCall{
		{"RemoteApplications", []interface{}{[]string{"mysql"}}},
	}
	s.waitForWorkerStubCalls(c, expected)
}

func (s *remoteRelationsSuite) assertRemoteRelationsWorkers(c *gc.C) worker.Worker {
	s.relationsFacade.relations["db2:db django:db"] = newMockRelation(123)
	w := s.assertRemoteApplicationWorkers(c)
	s.stub.ResetCalls()

	s.relationsFacade.relationsEndpoints["db2:db django:db"] = &relationEndpointInfo{
		localApplicationName: "django",
		localEndpoint: params.RemoteEndpoint{
			Name:      "db2",
			Role:      "requires",
			Interface: "db2",
		},
		remoteEndpointName: "data",
	}
	s.relationsFacade.controllerInfo["remote-model-uuid"] = &api.Info{
		Addrs: []string{"1.2.3.4:1234"}, CACert: coretesting.CACert}

	relWatcher, _ := s.relationsFacade.remoteApplicationRelationsWatcher("db2")
	relWatcher.changes <- []string{"db2:db django:db"}

	mac, err := macaroon.New(nil, "test", "")
	c.Assert(err, jc.ErrorIsNil)
	apiMac, err := macaroon.New(nil, "apimac", "")
	relTag := names.NewRelationTag("db2:db django:db")
	expected := []jujutesting.StubCall{
		{"Relations", []interface{}{[]string{"db2:db django:db"}}},
		{"ControllerAPIInfoForModel", []interface{}{"remote-model-uuid"}},
		{"ExportEntities", []interface{}{
			[]names.Tag{names.NewApplicationTag("django"), relTag}}},
		{"RegisterRemoteRelations", []interface{}{[]params.RegisterRemoteRelationArg{{
			ApplicationToken: "token-django",
			SourceModelTag:   "model-local-model-uuid",
			RelationToken:    "token-db2:db django:db",
			RemoteEndpoint: params.RemoteEndpoint{
				Name:      "db2",
				Role:      "requires",
				Interface: "db2",
			},
			OfferUUID:         "offer-db2-uuid",
			LocalEndpointName: "data",
			Macaroons:         macaroon.Slice{mac},
		}}}},
		{"SaveMacaroon", []interface{}{relTag, apiMac}},
		{"ImportRemoteEntity", []interface{}{names.NewApplicationTag("db2"), "token-offer-db2-uuid"}},
		{"WatchLocalRelationUnits", []interface{}{"db2:db django:db"}},
		{"WatchRelationUnits", []interface{}{"token-db2:db django:db", macaroon.Slice{apiMac}}},
		{"WatchRelationSuspendedStatus", []interface{}{"token-db2:db django:db", macaroon.Slice{apiMac}}},
	}
	s.waitForWorkerStubCalls(c, expected)

	unitWatcher, ok := s.relationsFacade.relationsUnitsWatcher("db2:db django:db")
	c.Check(ok, jc.IsTrue)
	waitForStubCalls(c, &unitWatcher.Stub, []jujutesting.StubCall{
		{"Changes", nil},
	})
	unitWatcher, ok = s.remoteRelationsFacade.relationsUnitsWatcher("token-db2:db django:db")
	c.Check(ok, jc.IsTrue)
	waitForStubCalls(c, &unitWatcher.Stub, []jujutesting.StubCall{
		{"Changes", nil},
	})
	relationStatusWatcher, ok := s.remoteRelationsFacade.relationsStatusWatcher("token-db2:db django:db")
	c.Check(ok, jc.IsTrue)
	waitForStubCalls(c, &relationStatusWatcher.Stub, []jujutesting.StubCall{
		{"Changes", nil},
	})
	return w
}

func (s *remoteRelationsSuite) TestRemoteRelationsWorkers(c *gc.C) {
	w := s.assertRemoteRelationsWorkers(c)
	workertest.CleanKill(c, w)

	// Check that relation unit watchers are stopped with the worker.
	relWatcher, ok := s.relationsFacade.relationsUnitsWatchers["db2:db django:db"]
	c.Check(ok, jc.IsTrue)
	c.Check(relWatcher.killed(), jc.IsTrue)

	relWatcher, ok = s.remoteRelationsFacade.relationsUnitsWatchers["token-db2:db django:db"]
	c.Check(ok, jc.IsTrue)
	c.Check(relWatcher.killed(), jc.IsTrue)
}

func (s *remoteRelationsSuite) TestRemoteRelationsDying(c *gc.C) {
	// Checks that when a remote relation dies, the relation units
	// workers are killed.
	w := s.assertRemoteRelationsWorkers(c)
	defer workertest.CleanKill(c, w)
	s.stub.ResetCalls()

	unitsWatcher, _ := s.relationsFacade.updateRelationLife("db2:db django:db", params.Dying)
	relWatcher, _ := s.relationsFacade.remoteApplicationRelationsWatcher("db2")
	relWatcher.changes <- []string{"db2:db django:db"}
	for a := coretesting.LongAttempt.Start(); a.Next(); {
		_, ok := s.relationsFacade.relationsUnitsWatcher("db2:db django:db")
		if ok {
			continue
		}
		_, ok = s.remoteRelationsFacade.relationsUnitsWatcher("token-db2:db django:db")
		if !ok {
			break
		}
		_, ok = s.remoteRelationsFacade.relationsStatusWatcher("token-db2:db django:db")
		if !ok {
			break
		}
	}
	c.Assert(unitsWatcher.killed(), jc.IsTrue)
	mac, err := macaroon.New(nil, "apimac", "")
	c.Assert(err, jc.ErrorIsNil)
	expected := []jujutesting.StubCall{
		{"Relations", []interface{}{[]string{"db2:db django:db"}}},
		{"GetToken", []interface{}{names.NewRelationTag("db2:db django:db")}},
		{"PublishRelationChange", []interface{}{
			params.RemoteRelationChangeEvent{
				Life:             params.Dying,
				ApplicationToken: "token-django",
				RelationToken:    "token-db2:db django:db",
				Macaroons:        macaroon.Slice{mac},
			},
		}},
	}
	s.waitForWorkerStubCalls(c, expected)
}

func (s *remoteRelationsSuite) TestLocalRelationsRemoved(c *gc.C) {
	// Checks that when a remote relation goes away, the relation units
	// worker is killed.
	w := s.assertRemoteRelationsWorkers(c)
	defer workertest.CleanKill(c, w)
	s.stub.ResetCalls()

	unitsWatcher, _ := s.relationsFacade.removeRelation("db2:db django:db")
	relWatcher, _ := s.relationsFacade.remoteApplicationRelationsWatcher("db2")
	relWatcher.changes <- []string{"db2:db django:db"}
	for a := coretesting.LongAttempt.Start(); a.Next(); {
		_, ok := s.relationsFacade.relationsUnitsWatcher("db2:db django:db")
		if !ok {
			break
		}
	}
	c.Assert(unitsWatcher.killed(), jc.IsTrue)
	mac, err := macaroon.New(nil, "apimac", "")
	c.Assert(err, jc.ErrorIsNil)
	expected := []jujutesting.StubCall{
		{"Relations", []interface{}{[]string{"db2:db django:db"}}},
		{"GetToken", []interface{}{names.NewRelationTag("db2:db django:db")}},
		{"PublishRelationChange", []interface{}{
			params.RemoteRelationChangeEvent{
				Life:             params.Dying,
				ApplicationToken: "token-django",
				RelationToken:    "token-db2:db django:db",
				Macaroons:        macaroon.Slice{mac},
			},
		}},
	}
	s.waitForWorkerStubCalls(c, expected)
}

func (s *remoteRelationsSuite) TestLocalRelationsChangedNotifies(c *gc.C) {
	w := s.assertRemoteRelationsWorkers(c)
	defer workertest.CleanKill(c, w)
	s.stub.ResetCalls()

	unitsWatcher, _ := s.relationsFacade.relationsUnitsWatcher("db2:db django:db")
	unitsWatcher.changes <- watcher.RelationUnitsChange{
		Changed:  map[string]watcher.UnitSettings{"unit/1": {Version: 2}},
		Departed: []string{"unit/2"},
	}

	mac, err := macaroon.New(nil, "apimac", "")
	c.Assert(err, jc.ErrorIsNil)
	expected := []jujutesting.StubCall{
		{"RelationUnitSettings", []interface{}{
			[]params.RelationUnit{{
				Relation: "relation-db2.db#django.db",
				Unit:     "unit-unit-1"}}}},
		{"PublishRelationChange", []interface{}{
			params.RemoteRelationChangeEvent{
				ApplicationToken: "token-django",
				RelationToken:    "token-db2:db django:db",
				ChangedUnits: []params.RemoteRelationUnitChange{{
					UnitId:   1,
					Settings: map[string]interface{}{"foo": "bar"},
				}},
				DepartedUnits: []int{2},
				Macaroons:     macaroon.Slice{mac},
			},
		}},
	}
	s.waitForWorkerStubCalls(c, expected)
}

func (s *remoteRelationsSuite) TestRemoteRelationsChangedConsumes(c *gc.C) {
	w := s.assertRemoteRelationsWorkers(c)
	defer workertest.CleanKill(c, w)
	s.stub.ResetCalls()

	unitsWatcher, _ := s.remoteRelationsFacade.relationsUnitsWatcher("token-db2:db django:db")
	unitsWatcher.changes <- watcher.RelationUnitsChange{
		Changed:  map[string]watcher.UnitSettings{"unit/1": {Version: 2}},
		Departed: []string{"unit/2"},
	}

	mac, err := macaroon.New(nil, "apimac", "")
	c.Assert(err, jc.ErrorIsNil)
	expected := []jujutesting.StubCall{
		{"RelationUnitSettings", []interface{}{
			[]params.RemoteRelationUnit{{
				RelationToken: "token-db2:db django:db",
				Unit:          "unit-unit-1",
				Macaroons:     macaroon.Slice{mac}}}}},
		{"ConsumeRemoteRelationChange", []interface{}{
			params.RemoteRelationChangeEvent{
				ApplicationToken: "token-offer-db2-uuid",
				RelationToken:    "token-db2:db django:db",
				ChangedUnits: []params.RemoteRelationUnitChange{{
					UnitId:   1,
					Settings: map[string]interface{}{"foo": "bar"},
				}},
				DepartedUnits: []int{2},
				Macaroons:     macaroon.Slice{mac},
			},
		}},
	}
	s.waitForWorkerStubCalls(c, expected)
}

func (s *remoteRelationsSuite) TestRemoteRelationsDyingConsumes(c *gc.C) {
	w := s.assertRemoteRelationsWorkers(c)
	defer workertest.CleanKill(c, w)
	s.stub.ResetCalls()

	statusWatcher, _ := s.remoteRelationsFacade.relationsStatusWatcher("token-db2:db django:db")
	statusWatcher.changes <- []watcher.RelationStatusChange{{
		Life: life.Dying,
	}}

	suspended := false
	expected := []jujutesting.StubCall{
		{"ConsumeRemoteRelationChange", []interface{}{
			params.RemoteRelationChangeEvent{
				Life:             params.Dying,
				ApplicationToken: "token-offer-db2-uuid",
				RelationToken:    "token-db2:db django:db",
				Suspended:        &suspended,
			},
		}},
	}
	s.waitForWorkerStubCalls(c, expected)
}

func (s *remoteRelationsSuite) TestRemoteRelationsChangedError(c *gc.C) {
	w := s.assertRemoteRelationsWorkers(c)
	// Just in case, ensure worker is killed.
	defer workertest.CheckKill(c, w)
	s.stub.ResetCalls()

	s.stub.SetErrors(errors.New("failed"))
	unitsWatcher, _ := s.relationsFacade.relationsUnitsWatcher("db2:db django:db")
	unitsWatcher.changes <- watcher.RelationUnitsChange{
		Departed: []string{"unit/1"},
	}
	err := workertest.CheckKilled(c, w)
	c.Assert(err, gc.ErrorMatches, "publishing relation change .* to remote model .*: failed")
}

func (s *remoteRelationsSuite) TestRegisteredApplicationNotRegistered(c *gc.C) {
	s.relationsFacade.relations["db2:db django:db"] = newMockRelation(123)
	db2app := newMockRemoteApplication("db2", "db2url")
	db2app.registered = true
	s.relationsFacade.remoteApplications["db2"] = db2app
	applicationNames := []string{"db2"}
	s.relationsFacade.remoteApplicationsWatcher.changes <- applicationNames

	w, err := remoterelations.New(s.config)
	c.Assert(err, jc.ErrorIsNil)
	defer workertest.CleanKill(c, w)

	expected := []jujutesting.StubCall{
		{"WatchRemoteApplications", nil},
		{"RemoteApplications", []interface{}{[]string{"db2"}}},
		{"WatchRemoteApplicationRelations", []interface{}{"db2"}},
	}
	s.waitForWorkerStubCalls(c, expected)
	s.stub.ResetCalls()

	s.relationsFacade.relationsEndpoints["db2:db django:db"] = &relationEndpointInfo{
		localApplicationName: "django",
		localEndpoint: params.RemoteEndpoint{
			Name:      "db2",
			Role:      "requires",
			Interface: "db2",
		},
		remoteEndpointName: "data",
	}

	relWatcher, _ := s.relationsFacade.remoteApplicationRelationsWatcher("db2")
	relWatcher.changes <- []string{"db2:db django:db"}

	expected = []jujutesting.StubCall{
		{"Relations", []interface{}{[]string{"db2:db django:db"}}},
	}
	s.waitForWorkerStubCalls(c, expected)
}
