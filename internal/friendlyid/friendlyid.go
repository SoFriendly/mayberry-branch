package friendlyid

import (
	"fmt"
	"math/rand"
)

var adjectives = []string{
	"sunny", "quiet", "golden", "mossy", "amber", "crystal", "dusty",
	"gentle", "hidden", "ivory", "jolly", "keen", "little", "merry",
	"noble", "olive", "proud", "rosy", "silver", "tender",
}

var nouns = []string{
	"oak", "elm", "birch", "cedar", "maple", "pine", "willow",
	"brook", "creek", "glen", "hill", "lake", "meadow", "ridge",
	"stone", "trail", "vale", "harbor", "garden", "lantern",
}

// Generate creates a friendly ID like "sunny-oak".
func Generate() string {
	a := adjectives[rand.Intn(len(adjectives))]
	n := nouns[rand.Intn(len(nouns))]
	return fmt.Sprintf("%s-%s", a, n)
}
