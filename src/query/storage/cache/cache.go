package cache

type Cache struct {
	Address string
}

func NewCache(cacheOpt *CacheOptions) *Cache {
	if cacheOpt == nil || cacheOpt.RedisAddress == nil {
		return nil
	}
	return &Cache{
		Address: *cacheOpt.RedisAddress,
	}
}
