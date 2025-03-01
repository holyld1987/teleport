/*
Copyright 2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package users

import (
	"context"
	"time"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/sirupsen/logrus"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/api/utils/retryutils"
	"github.com/gravitational/teleport/lib/cloud"
	"github.com/gravitational/teleport/lib/utils/interval"
)

// Config is the config for users service.
type Config struct {
	// Clients is an interface for retrieving cloud clients.
	Clients cloud.Clients
	// Clock is used to control time.
	Clock clockwork.Clock
	// Interval is the interval between user updates. Interval is also used as
	// the minimum password expiration duration.
	Interval time.Duration
	// Log is the logrus field logger.
	Log logrus.FieldLogger
	// UpdateMeta is used to update database metadata.
	UpdateMeta func(context.Context, types.Database) error
}

// CheckAndSetDefaults validates the config and set defaults.
func (c *Config) CheckAndSetDefaults() error {
	if c.UpdateMeta == nil {
		return trace.BadParameter("missing UpdateMeta")
	}
	if c.Clients == nil {
		c.Clients = cloud.NewClients()
	}
	if c.Clock == nil {
		c.Clock = clockwork.NewRealClock()
	}
	if c.Interval == 0 {
		// An AWS Secrets Manager secret can have at most 100 versions per day.
		// That is 14 minutes and 24 seconds per version at minimum. Using 15
		// minutes here to be safe. Also with the extra jitter added, the real
		// average on rotation will be over 16 minutes apart.
		//
		// https://docs.aws.amazon.com/secretsmanager/latest/userguide/reference_limits.html
		//
		// Note that currently all database types are sharing the same interval
		// for password rotations.
		c.Interval = 15 * time.Minute
	}
	if c.Log == nil {
		c.Log = logrus.WithField(trace.Component, "clouduser")
	}
	return nil
}

// Users manages database users for cloud databases.
type Users struct {
	// cfg is the config for users service.
	cfg Config
	// fetchersByType is a map of fetchers by database type.
	fetchersByType map[string]Fetcher
	// usersByID owns and tracks a map by unique users by their IDs. User's
	// setup/teardown is performed when user is added to/removed from the map.
	usersByID map[string]User
	// lookup is used to track mappings between database and their users.
	lookup *lookupMap
	// setupDatabaseChan is the channel used for setting up a database.
	setupDatabaseChan chan types.Database
}

// Fetcher fetches database users for a particular database type.
type Fetcher interface {
	// GetType returns the database type of the fetcher.
	GetType() string
	// FetchDatabaseUsers fetches users for provided database.
	FetchDatabaseUsers(ctx context.Context, database types.Database) ([]User, error)
}

// NewUsers returns a new instance of users service.
func NewUsers(cfg Config) (*Users, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	fetchersByType, err := makeFetchers(cfg)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &Users{
		cfg:               cfg,
		fetchersByType:    fetchersByType,
		lookup:            newLookupMap(),
		usersByID:         make(map[string]User),
		setupDatabaseChan: make(chan types.Database),
	}, nil
}

// GetPassword returns the password for database login.
func (u *Users) GetPassword(ctx context.Context, database types.Database, username string) (string, error) {
	user, found := u.lookup.getDatabaseUser(database, username)
	if !found {
		return "", trace.NotFound("database user %s is not managed", username)
	}

	return user.GetPassword(ctx)
}

// Setup starts to discover and manage users for provided database.
//
// Setup allows managed database users to become available as soon as new
// database is registered instead of waiting for the periodic setup goroutine.
// Note that there is no corresponding "Teardown" as cleanup will eventually
// happen in the periodic setup.
func (u *Users) Setup(_ context.Context, database types.Database) error {
	_, found := u.fetchersByType[database.GetType()]
	if !found {
		return nil
	}

	u.setupDatabaseChan <- database
	return nil
}

// Start starts users service to manage cloud database users.
func (u *Users) Start(ctx context.Context, getAllDatabases func() types.Databases) {
	u.cfg.Log.Debug("Starting cloud users service.")
	defer u.cfg.Log.Debug("Cloud users service done.")

	ticker := interval.New(interval.Config{
		// Use jitter for HA setups.
		Jitter: retryutils.NewSeventhJitter(),

		// NewSeventhJitter builds a new jitter on the range [6n/7,n).
		// Use n = cfg.Interval*7/6 gives an effective duration range of
		// [cfg.Interval, cfg.Interval*7/6), to ensure minimum is cfg.Interval.
		// The extra jitter also helps offset small clock skews.
		Duration: u.cfg.Interval * 7 / 6,
	})
	defer ticker.Stop()

	for {
		select {
		case database := <-u.setupDatabaseChan:
			u.setupDatabaseAndRotatePasswords(ctx, database)

		case <-ticker.Next():
			u.setupAllDatabasesAndRotatePassowrds(ctx, getAllDatabases())

		case <-ctx.Done():
			return
		}
	}
}

// setupDatabaseAndRotatePasswords performs setup for a single database.
func (u *Users) setupDatabaseAndRotatePasswords(ctx context.Context, database types.Database) {
	// Database metadata is already refreshed once during database
	// registration so no need to do it again.
	u.setupDatabasesAndRotatePasswords(ctx, types.Databases{database}, false /*updateMeta*/)
}

// setupAllDatabasesAndRotatePassowrds performs setup for all databases.
func (u *Users) setupAllDatabasesAndRotatePassowrds(ctx context.Context, allDatabases types.Databases) {
	u.setupDatabasesAndRotatePasswords(ctx, allDatabases, true)

	// Clean up.
	u.lookup.removeUnusedDatabases(allDatabases)
	activeUsers := u.lookup.usersByID()
	for userID, user := range u.usersByID {
		if _, found := activeUsers[userID]; !found {
			delete(u.usersByID, userID)

			if err := user.Teardown(ctx); err != nil {
				u.cfg.Log.WithError(err).Errorf("Failed to tear down user %v.", user)
			}
		}
	}
}

// setupDatabasesAndRotatePasswords performs setup for provided databases and
// rotate user passwords.
func (u *Users) setupDatabasesAndRotatePasswords(ctx context.Context, databases types.Databases, updateMeta bool) {
	for _, database := range databases {
		fetcher, found := u.fetchersByType[database.GetType()]
		if !found {
			continue
		}

		// Refresh database metadata like ElastiCache user group IDs.
		//
		// Auto discovered databases can be skipped as the watcher service
		// discovers changes periodically then triggers reconciler.
		//
		// If UpdateMeta fails, log an error and continue to next step with
		// whatever meta database already has.
		if updateMeta && database.Origin() != types.OriginCloud {
			if err := u.cfg.UpdateMeta(ctx, database); err != nil {
				u.cfg.Log.WithError(err).Errorf("Failed to update metadata for %q.", database)
			}
		}

		// Fetch users.
		fetchedUsers, err := fetcher.FetchDatabaseUsers(ctx, database)
		if err != nil {
			if trace.IsAccessDenied(err) { // Permission errors are expected.
				u.cfg.Log.WithError(err).Debugf("No permissions to fetch users for %q.", database)
			} else {
				u.cfg.Log.WithError(err).Errorf("Failed to fetch users for database %v.", database)
			}
			continue
		}

		// Setup users.
		var users []User
		for _, fetchedUser := range fetchedUsers {
			if user, err := u.setupUser(ctx, fetchedUser); err != nil {
				u.cfg.Log.WithError(err).Errorf("Failed to setup user %v for database %v.", fetchedUser, database)
			} else {
				users = append(users, user)
			}
		}

		// Rotate passwords.
		for _, user := range users {
			if err = user.RotatePassword(ctx); err != nil {
				u.cfg.Log.WithError(err).Errorf("Failed to rotate password for user %v", user)
			}
		}

		// Update lookup.
		u.lookup.setDatabaseUsers(database, users)
	}
}

// setupUser finds existing user if it is already managed and tracked,
// otherwise try to setup the new user.
func (u *Users) setupUser(ctx context.Context, user User) (User, error) {
	if existingUser, found := u.usersByID[user.GetID()]; found {
		// TODO(greedy52) may want to compare secret store setting in case they
		// are different.
		return existingUser, nil
	}

	if err := user.Setup(ctx); err != nil {
		return nil, trace.Wrap(err)
	}

	u.usersByID[user.GetID()] = user
	return user, nil
}

// makeFetchers create a map of fetchers by their types.
func makeFetchers(cfg Config) (map[string]Fetcher, error) {
	newFetcherFuncs := []func(Config) (Fetcher, error){
		newElastiCacheFetcher,
		newMemoryDBFetcher,
	}

	fetchersByType := make(map[string]Fetcher)
	for _, newFetcherFunc := range newFetcherFuncs {
		fetcher, err := newFetcherFunc(cfg)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		fetchersByType[fetcher.GetType()] = fetcher
	}
	return fetchersByType, nil
}
