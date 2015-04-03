package mongodump

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/mongodb/mongo-tools/common/bsonutil"
	"github.com/mongodb/mongo-tools/common/db"
	"github.com/mongodb/mongo-tools/common/intents"
	"github.com/mongodb/mongo-tools/common/log"
	"gopkg.in/mgo.v2/bson"
)

// Metadata holds information about a collection's options and indexes.
type Metadata struct {
	Options interface{}   `json:"options,omitempty"`
	Indexes []interface{} `json:"indexes"`
}

// IndexDocumentFromDB is used internally to preserve key ordering.
type IndexDocumentFromDB struct {
	Options bson.M `bson:",inline"`
	Key     bson.D `bson:"key"`
}

// dumpMetadataToWriter gets the metadata for a collection and writes it
// in readable JSON format.
func (dump *MongoDump) dumpMetadata(intent *intents.Intent) error {
	err := intent.OpenMetadata(intent)
	if err != nil {
		return err
	}
	// make a buffered writer for nicer disk i/o
	w := bufio.NewWriter(intent.MetadataFile)

	nsID := fmt.Sprintf("%v.%v", intent.DB, intent.C)
	meta := Metadata{
		// We have to initialize Indexes to an empty slice, not nil, so that an empty
		// array is marshalled into json instead of null. That is, {indexes:[]} is okay
		// but {indexes:null} will cause assertions in our legacy C++ mongotools
		Indexes: []interface{}{},
	}

	// First, we get the options for the collection. These are pulled
	// using either listCollections (2.7+) or by querying system.namespaces
	// (2.6 and earlier), the internal collection containing collection names
	// and properties. For mongodump, we copy just the "options"
	// subdocument for the collection.
	log.Logf(log.DebugHigh, "\treading options for `%v`", nsID)

	session, err := dump.sessionProvider.GetSession()
	if err != nil {
		return err
	}
	defer session.Close()
	session.SetSocketTimeout(0)
	collection := session.DB(intent.DB).C(intent.C)

	collectionInfo, err := db.GetCollectionOptions(collection)
	if err != nil {
		return fmt.Errorf("failed to get collection options for `%v`: %v", err)
	}
	if collectionInfo == nil {
		// The collection wasn't found, which means it was probably deleted
		// between now and the time that collections were listed. Skip it.
		log.Logf(log.DebugLow, "Warning: no metadata found for collection: `%v`: %v", nsID, err)
		return nil
	}
	meta.Options = bsonutil.MarshalD{}
	if opts, err := bsonutil.FindValueByKey("options", collectionInfo); err == nil {
		if optsD, ok := opts.(bson.D); ok {
			// make the options properly json-able
			meta.Options, err = bsonutil.ConvertBSONValueToJSON(optsD)
			if err != nil {
				return fmt.Errorf("error converting collection options to JSON: %v", err)
			}
		} else {
			return fmt.Errorf("collection options contains invalid data: %v", opts)
		}
	}

	// Second, we read the collection's index information by either calling
	// listIndexes (pre-2.7 systems) or querying system.indexes.
	// We keep a running list of all the indexes
	// for the current collection as we iterate over the cursor, and include
	// that list as the "indexes" field of the metadata document.
	log.Logf(log.DebugHigh, "\treading indexes for `%v`", nsID)

	// get the indexes
	indexesIter, err := db.GetIndexes(collection)
	if err != nil {
		return err
	}

	indexOpts := &bson.D{}
	for indexesIter.Next(indexOpts) {
		convertedIndex, err := bsonutil.ConvertBSONValueToJSON(*indexOpts)
		if err != nil {
			return fmt.Errorf("error converting index (%#v): %v", convertedIndex, err)
		}
		meta.Indexes = append(meta.Indexes, convertedIndex)
	}

	if err := indexesIter.Err(); err != nil {
		return fmt.Errorf("error getting indexes for collection `%v`: %v", nsID, err)
	}

	// Finally, we send the results to the writer as JSON bytes
	jsonBytes, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("error marshalling metadata json for collection `%v`: %v", nsID, err)
	}
	_, err = w.Write(jsonBytes)
	if err != nil {
		return fmt.Errorf("error writing metadata for collection `%v` to disk: %v", nsID, err)
	}
	err = w.Flush()
	if err != nil {
		return fmt.Errorf("error writing metadata for collection `%v` to disk: %v", nsID, err)
	}
	return nil
}
