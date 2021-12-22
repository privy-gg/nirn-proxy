package lib

import (
	"errors"
	"github.com/Clever/leakybucket"
	"github.com/Clever/leakybucket/memory"
	"github.com/sirupsen/logrus"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type QueueItem struct {
	Req *http.Request
	Res *http.ResponseWriter
	doneChan chan *http.Response
	errChan chan error
}

type QueueChannel struct {
	ch chan *QueueItem
	lastUsed *time.Time
}

type RequestQueue struct {
	sync.RWMutex
	channelMu         sync.Mutex
	globalLockedUntil *int64
	sweepTicker       *time.Ticker
	// bucket path hash as key
	queues map[uint64]*QueueChannel
	processor func(item *QueueItem) (*http.Response, error)
	globalBucket leakybucket.Bucket
	// bufferSize Defines the size of the request channel buffer for each bucket
	bufferSize int64
}

func NewRequestQueue(processor func(item *QueueItem) (*http.Response, error), globalLimit uint, bufferSize int64) *RequestQueue {
	memStorage := memory.New()
	globalBucket, err := memStorage.Create("global", globalLimit, 1 * time.Second)
	if err != nil {
		panic(err)
	}

	ret := &RequestQueue{
		queues:            make(map[uint64]*QueueChannel),
		processor:         processor,
		globalBucket:      globalBucket,
		globalLockedUntil: new(int64),
		bufferSize: 	   bufferSize,
	}
	go ret.tickSweep()
	return ret
}
func (q *RequestQueue) sweep() {
	q.Lock()
	q.channelMu.Lock()
	defer q.Unlock()
	defer q.channelMu.Unlock()
	logger.Info("Sweep start")
	sweptEntries := 0
	for key, val := range q.queues {
		if time.Since(*val.lastUsed) > 10 * time.Minute {
			close(val.ch)
			delete(q.queues, key)
			sweptEntries++
		}
	}
	logger.WithFields(logrus.Fields{"sweptEntries": sweptEntries}).Info("Finished sweep")
}

func (q *RequestQueue) tickSweep() {
	q.sweepTicker = time.NewTicker(5 * time.Minute)

	for range q.sweepTicker.C {
		q.sweep()
	}
}

func (q *RequestQueue) Queue(req *http.Request, res *http.ResponseWriter) (string, *http.Response, error) {
	path := GetOptimisticBucketPath(req.URL.Path, req.Method)
	logger.WithFields(logrus.Fields{
		"bucket": path,
		"path": req.URL.Path,
		"method": req.Method,
	}).Trace("Inbound request")
	q.RLock()
	ch := q.getQueueChannel(path)

	doneChan := make(chan *http.Response)
	errChan := make(chan error)
	ch.ch <- &QueueItem{req, res, doneChan, errChan }
	q.RUnlock()
	select {
	case resp := <-doneChan:
		return path, resp, nil
	case err := <-errChan:
		return path, nil, err
	}
}

func (q *RequestQueue) getQueueChannel(path string) *QueueChannel {
	pathHash := HashCRC64(path)
	q.channelMu.Lock()
	defer q.channelMu.Unlock()
	t := time.Now()
	ch, ok := q.queues[pathHash]
	if !ok {
		// Check again to see if the queue channel wasn't created
		// While we didn't hold the exclusive lock
		ch, ok = q.queues[pathHash]
		if !ok {
			ch = &QueueChannel{ make(chan *QueueItem, q.bufferSize), &t }
			q.queues[pathHash] = ch
			// It's important that we only have 1 goroutine per channel
			go q.subscribe(ch, path, pathHash)
		}
	} else {
		ch.lastUsed = &t
	}
	return ch
}

func parseHeaders(headers *http.Header) (int64, int64, time.Duration, bool, error) {
	if headers == nil {
		return 0, 0, 0, false, errors.New("null headers")
	}

	limit := headers.Get("x-ratelimit-limit")
	remaining := headers.Get("x-ratelimit-remaining")
	resetAfter := headers.Get("x-ratelimit-reset-after")
	if resetAfter == "" {
		// Globals return no x-ratelimit-reset-after headers, this is the best option without parsing the body
		resetAfter = headers.Get("retry-after")
	}
	isGlobal := headers.Get("x-ratelimit-global") == "true"

	var resetParsed float64
	var reset time.Duration
	var err error
	if resetAfter != "" {
		resetParsed, err = strconv.ParseFloat(resetAfter, 64)
		if err != nil {
			return 0, 0, 0, false, err
		}

		// Convert to MS instead of seconds to preserve decimal precision
		reset = time.Duration(int(resetParsed * 1000)) * time.Millisecond
	}

	if isGlobal {
		return 0, 0, reset, isGlobal, nil
	}

	if limit == "" {
		return 0, 0, 0, false, nil
	}

	limitParsed, err := strconv.ParseInt(limit, 10, 32)
	if err != nil {
		return 0, 0, 0, false, err
	}

	remainingParsed, err := strconv.ParseInt(remaining, 10, 32)
	if err != nil {
		return 0, 0, 0, false, err
	}

	return limitParsed, remainingParsed, reset, isGlobal, nil
}

func (q *RequestQueue) takeGlobal(path string) {
takeGlobal:
	waitTime := atomic.LoadInt64(q.globalLockedUntil)
	if waitTime > 0 {
		logger.WithFields(logrus.Fields{
			"bucket": path,
			"waitTime": waitTime,
		}).Trace("Waiting for existing global to clear")
		time.Sleep(time.Until(time.Unix(0, waitTime)))
		sw := atomic.CompareAndSwapInt64(q.globalLockedUntil, waitTime, 0)
		if sw {
			logger.Info("Unlocked global bucket")
		}
	}
	_, err := q.globalBucket.Add(1)
	if err != nil {
		reset := q.globalBucket.Reset()
		logger.WithFields(logrus.Fields{
			"bucket": path,
			"waitTime": time.Until(reset),
		}).Trace("Failed to grab global token, sleeping for a bit")
		time.Sleep(time.Until(reset))
		goto takeGlobal
	}
}

func return404webhook(item *QueueItem) {
	res := *item.Res
	res.WriteHeader(404)
	body := "{\n  \"message\": \"Unknown Webhook\",\n  \"code\": 10015\n}"
	_, err := res.Write([]byte(body))
	if err != nil {
		return
	}
}

func isInteraction(url string) bool {
	parts := strings.Split(strings.SplitN(url, "?", 1)[0], "/")
	for _, p := range parts {
		if len(p) > 128 {
			return true
		}
	}
	return false
}

func (q *RequestQueue) subscribe(ch *QueueChannel, path string, pathHash uint64) {
	// This function has 1 goroutine for each bucket path
	// Locking here is not needed

	//Only used for logging
	var prevRem int64 = 0
	var prevReset time.Duration = 0

	// Fail fast path for webhook 404s
	var ret404 = false
	for item := range ch.ch {
		if ret404 {
			return404webhook(item)
			item.doneChan <- nil
			continue
		}

		q.takeGlobal(path)

		resp, err := q.processor(item)
		if err != nil {
			item.errChan <- err
			continue
		}

		_, remaining, resetAfter, isGlobal, err := parseHeaders(&resp.Header)

		if isGlobal {
			//Lock global
			sw := atomic.CompareAndSwapInt64(q.globalLockedUntil, 0, time.Now().Add(resetAfter).UnixNano())
			if sw {
				logger.WithFields(logrus.Fields{
					"until": time.Now().Add(resetAfter),
					"resetAfter": resetAfter,
				}).Warn("Global reached, locking")
			}
		}

		if err != nil {
			item.errChan <- err
			continue
		}
		item.doneChan <- resp

		if resp.StatusCode == 429 {
			logger.WithFields(logrus.Fields{
				"prevRemaining": prevRem,
				"prevResetAfter": prevReset,
				"remaining": remaining,
				"resetAfter": resetAfter,
				"bucket": path,
				"route": item.Req.URL.String(),
				"method": item.Req.Method,
				"isGlobal": isGlobal,
				"pathHash": pathHash,
				// TODO: Remove this when 429s are not a problem anymore
				"discordBucket": resp.Header.Get("x-ratelimit-bucket"),
				"ratelimitScope": resp.Header.Get("x-ratelimit-scope"),
			}).Warn("Unexpected 429")
		}

		if resp.StatusCode == 404 && strings.HasPrefix(path, "/webhooks/") && !isInteraction(item.Req.URL.String()) {
			logger.WithFields(logrus.Fields{
				"bucket": path,
				"route": item.Req.URL.String(),
				"method": item.Req.Method,
			}).Info("Setting fail fast 404 for webhook")
			ret404 = true
		}
		if remaining == 0 || resp.StatusCode == 429 {
			time.Sleep(time.Until(time.Now().Add(resetAfter)))
		}
		prevRem, prevReset = remaining, resetAfter
	}
}