package main

import (
	"go.mongodb.org/mongo-driver/v2/bson"
)

// toObjectID parses a hex string into an ObjectID.
func toObjectID(hex string) (bson.ObjectID, error) {
	return bson.ObjectIDFromHex(hex)
}
