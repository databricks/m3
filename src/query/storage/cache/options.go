package cache

// If a duration falls within (start, end], use bucket as size
type BucketRange struct {
	start  int `yaml:"start"`
	end    int `yaml:"end"`
	bucket int `yaml:"bucket"`
}

type BucketRules struct {
	Rules []*BucketRange `yaml:"rules"`
	// If bucket doesn't fall in any range, use default
	DefaultBucket int `yaml:"default"`
}

// Get the bucket size to use for this duration length
func (b *BucketRules) findBucketSize(duration int) int {
	for _, bucket := range b.Rules {
		if duration <= bucket.end && duration > bucket.start {
			return bucket.bucket
		}
	}
	return b.DefaultBucket
}

// Cache options
type CacheOptions struct {
	// RedisAddress is the Redis server address.
	RedisAddress *string `yaml:"redisAddress"`
}
