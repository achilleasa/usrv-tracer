package storage

import (
	"time"

	"encoding/json"
	"fmt"

	"sort"

	redisAdapter "github.com/achilleasa/usrv-service-adapters/service/redis"
	"github.com/achilleasa/usrv-tracer"
	"github.com/garyburd/redigo/redis"
)

// Redis is a singleton instance of a redis-backed storage service
var Redis *redisStorage = &redisStorage{
	redisSrv: redisAdapter.Adapter,
}

// This storage backend is built on top of Redis. Internally it uses
// a connection pool to provide thread-safe access.
type redisStorage struct {
	redisSrv *redisAdapter.Redis
}

// Dial the storage
func (r *redisStorage) Dial() error {
	return r.redisSrv.Dial()
}

// Store a trace entry and set a TTL on it. If the ttl is 0 then the
// trace record will never expire. Implements the Storage interface.
func (r *redisStorage) Store(logEntry *tracer.Record, ttl time.Duration) error {
	json, err := json.Marshal(logEntry)
	if err != nil {
		return err
	}

	conn, err := r.redisSrv.GetConnection()
	if err != nil {
		return err
	}
	defer conn.Close()

	conn.Send("MULTI")

	// Append log entry to a list that shares the same traceId
	// and set a TTL
	traceKey := fmt.Sprintf("tracer.%s", logEntry.TraceId)
	conn.Send("LPUSH", traceKey, json)
	if ttl > time.Second {
		conn.Send("EXPIRE", traceKey, ttl.Seconds())
	}

	// Add logEntry.From to the set of known services
	conn.Send("SADD", "tracer.services", logEntry.From)

	// If this is an outgoing request, add the destination to the dependency set
	// for the origin
	if logEntry.Type == tracer.Request {
		conn.Send("SADD", fmt.Sprintf("tracer.%s.deps", logEntry.From), logEntry.To)
	}

	// Exec pipeline
	_, err = conn.Do("EXEC")
	return err
}

// Fetch a set of time-ordered trace entries with the given trace-id.
func (r *redisStorage) GetTrace(traceId string) (tracer.Trace, error) {

	conn, err := r.redisSrv.GetConnection()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Get the number of records
	traceKey := fmt.Sprintf("tracer.%s", traceId)
	len, err := redis.Int(conn.Do("LLEN", traceKey))
	if err != nil {
		return nil, err
	}

	// Fetch all records
	rawRows, err := redis.Strings(conn.Do("LRANGE", traceKey, 0, len))
	if err != nil {
		return nil, err
	}

	// Unmarshal raw data
	traceLog := make(tracer.Trace, len)
	for index, rawRow := range rawRows {
		entry := tracer.Record{}
		err = json.Unmarshal([]byte(rawRow), &entry)
		if err != nil {
			return nil, err
		}
		traceLog[index] = entry
	}

	// Sort the trace so entries appear in insertion order
	sort.Sort(traceLog)

	return traceLog, nil
}

// Get service dependencies optionally filtered by a set of service names. If no filters are
// specified then the response will include all services currently known to the storage.
func (r *redisStorage) GetDependencies(srvFilter ...string) ([]tracer.Dependencies, error) {
	conn, err := r.redisSrv.GetConnection()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if len(srvFilter) == 0 {
		srvFilter, err = redis.Strings(conn.Do("SMEMBERS", "tracer.services"))
		if err != nil {
			return nil, err
		}
		sort.Strings(srvFilter)
	}

	// Fetch deps in a single batch
	conn.Send("MULTI")
	for _, serviceName := range srvFilter {
		conn.Send("SMEMBERS", fmt.Sprintf("tracer.%s.deps", serviceName))
	}
	replies, err := redis.Values(conn.Do("EXEC"))
	if err != nil {
		return nil, err
	}

	// Assemble deps
	replyCount := len(srvFilter)
	serviceDeps := make([]tracer.Dependencies, replyCount)
	for index := 0; index < replyCount; index++ {
		deps, _ := redis.Strings(replies[index], nil)
		serviceDeps[index] = tracer.Dependencies{
			Service:      srvFilter[index],
			Dependencies: deps,
		}
	}

	return serviceDeps, nil
}

// Shutdown the storage.
func (r *redisStorage) Close() {
	r.redisSrv.Close()
}
