package mongodump

import (
	"fmt"
	"github.com/mongodb/mongo-tools/common/intents"
	"github.com/mongodb/mongo-tools/common/log"
	"os"
	"path/filepath"
	"strings"
)

type bsonFileFile struct {
	*os.File
	intent *intents.Intent
}

func (f *bsonFileFile) Open() (err error) {
	if f.intent.BSONPath == "" {
		return fmt.Errorf("No BSONPath for %v.%v", f.intent.DB, f.intent.C)
	}
	f.File, err = os.Create(f.intent.BSONPath)
	if err != nil {
		return fmt.Errorf("error creating BSON file %v: %v", f.intent.BSONPath, err)
	}
	return nil
}

type metadataFileFile struct {
	*os.File
	intent *intents.Intent
}

func (f *metadataFileFile) Open() (err error) {
	if f.intent.MetadataPath == "" {
		return fmt.Errorf("No MetadataPath for %v.%v", f.intent.DB, f.intent.C)
	}
	f.File, err = os.Create(f.intent.MetadataPath)
	if err != nil {
		return fmt.Errorf("error creating Metadata file %v: %v", f.intent.MetadataPath, err)
	}
	return nil
}

type stdoutFile struct {
	*os.File
	intent *intents.Intent
}

func (f *stdoutFile) Open() error {
	f.File = os.Stdout
	return nil
}

func (f *stdoutFile) Close() error {
	f.File = nil
	return nil
}

func (f *stdoutFile) Read(p []byte) (n int, err error) {
	return 0, fmt.Errorf("can't read from standard output")
}

// shouldSkipCollection returns true when a collection name is excluded
// by the mongodump options.
func (dump *MongoDump) shouldSkipCollection(colName string) bool {
	for _, excludedCollection := range dump.OutputOptions.ExcludedCollections {
		if colName == excludedCollection {
			return true
		}
	}
	for _, excludedCollectionPrefix := range dump.OutputOptions.ExcludedCollectionPrefixes {
		if strings.HasPrefix(colName, excludedCollectionPrefix) {
			return true
		}
	}
	return false
}

// outputPath creates a path for the collection to be written to (sans file extension).
func (dump *MongoDump) outputPath(dbName, colName string) string {
	return filepath.Join(dump.OutputOptions.Out, dbName, colName)
}

func (dump *MongoDump) CreateOplogIntents() error {

	err := dump.determineOplogCollectionName()
	if err != nil {
		return err
	}

	err = os.MkdirAll(dump.OutputOptions.Out, defaultPermissions)
	if err != nil {
		return err
	}

	oplogIntent := &intents.Intent{
		DB:       "local",
		C:        dump.oplogCollection,
		BSONPath: filepath.Join(dump.OutputOptions.Out, "oplog.bson"),
	}
	oplogIntent.BSONFile = &bsonFileFile{intent: oplogIntent}
	dump.manager.Put(oplogIntent)
	return nil
}

// CreateUsersRolesVersionIntentsForDB create intents to be written in to the specific
// collection folder, for the users, roles and version admin database collections
// And then it adds the intents in to the manager
func (dump *MongoDump) CreateUsersRolesVersionIntentsForDB(db string) error {

	outDir := filepath.Join(dump.OutputOptions.Out, db)
	err := os.MkdirAll(outDir, defaultPermissions)
	if err != nil {
		return err
	}

	usersIntent := &intents.Intent{
		DB:       "admin",
		C:        "system.users",
		BSONPath: filepath.Join(outDir, "$admin.system.users.bson"),
	}
	usersIntent.BSONFile = &bsonFileFile{intent: usersIntent}
	dump.manager.Put(usersIntent)

	rolesIntent := &intents.Intent{
		DB:       "admin",
		C:        "system.roles",
		BSONPath: filepath.Join(outDir, "$admin.system.roles.bson"),
	}
	rolesIntent.BSONFile = &bsonFileFile{intent: rolesIntent}
	dump.manager.Put(rolesIntent)

	versionIntent := &intents.Intent{
		DB:       "admin",
		C:        "system.version",
		BSONPath: filepath.Join(outDir, "$admin.system.version.bson"),
	}
	versionIntent.BSONFile = &bsonFileFile{intent: versionIntent}
	dump.manager.Put(versionIntent)

	return nil
}

// CreateIntentsForCollection builds an intent for a given collection and
// puts it into the intent manager.
func (dump *MongoDump) CreateIntentForCollection(dbName, colName string) error {
	if dump.shouldSkipCollection(colName) {
		log.Logf(log.DebugLow, "skipping dump of %v.%v, it is excluded", dbName, colName)
		return nil
	}

	outDir := filepath.Join(dump.OutputOptions.Out, dbName)
	err := os.MkdirAll(outDir, defaultPermissions)
	if err != nil {
		return err
	}

	intent := &intents.Intent{
		DB:       dbName,
		C:        colName,
		BSONPath: dump.outputPath(dbName, colName+".bson"),
	}
	intent.BSONFile = &bsonFileFile{intent: intent}

	if !intent.IsSystemIndexes() {
		intent.MetadataPath = dump.outputPath(dbName, colName+".metadata.json")
		intent.MetadataFile = &metadataFileFile{intent: intent}
	}

	// add stdout flags if we're using stdout
	if dump.useStdout {
		intent.BSONFile = &stdoutFile{intent: intent}
		// We don't actually need a stdoutMetadataFile type because none of the methods on the stdoutFile
		// Make any use of the BSON or Metadata parts of the intent
		intent.MetadataFile = &stdoutFile{intent: intent}
	}

	// get a document count for scheduling purposes
	session, err := dump.sessionProvider.GetSession()
	if err != nil {
		return err
	}
	defer session.Close()

	count, err := session.DB(dbName).C(colName).Count()
	if err != nil {
		return fmt.Errorf("error counting %v: %v", intent.Namespace(), err)
	}
	intent.Size = int64(count)
	dump.manager.Put(intent)

	log.Logf(log.DebugLow, "enqueued collection '%v'", intent.Namespace())

	return nil
}

// CreateIntentsForDatabase iterates through collections in a db
// and builds dump intents for each collection.
func (dump *MongoDump) CreateIntentsForDatabase(dbName string) error {
	// we must ensure folders for empty databases are still created, for legacy purposes
	dbFolder := filepath.Join(dump.OutputOptions.Out, dbName)
	err := os.MkdirAll(dbFolder, defaultPermissions)
	if err != nil {
		return fmt.Errorf("error creating directory `%v`: %v", dbFolder, err)
	}

	cols, err := dump.sessionProvider.CollectionNames(dbName)
	if err != nil {
		return fmt.Errorf("error getting collection names for database `%v`: %v", dbName, err)
	}

	log.Logf(log.DebugHigh, "found collections: %v", strings.Join(cols, ", "))
	for _, colName := range cols {
		if err = dump.CreateIntentForCollection(dbName, colName); err != nil {
			return err // no context needed
		}
	}
	return nil
}

// CreateAllIntents iterates through all dbs and collections and builds
// dump intents for each collection.
func (dump *MongoDump) CreateAllIntents() error {
	dbs, err := dump.sessionProvider.DatabaseNames()
	if err != nil {
		return fmt.Errorf("error getting database names: %v", err)
	}
	log.Logf(log.DebugHigh, "found databases: %v", strings.Join(dbs, ", "))
	for _, dbName := range dbs {
		if dbName == "local" {
			// local can only be explicitly dumped
			continue
		}
		if err := dump.CreateIntentsForDatabase(dbName); err != nil {
			return err
		}
	}
	return nil
}
