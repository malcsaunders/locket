package locket

import (
	"errors"
	"os"
	"time"

	"github.com/cloudfoundry-incubator/consuladapter"
	"github.com/cloudfoundry-incubator/runtime-schema/metric"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"
)

var (
	ErrLockLost = errors.New("lock lost")
)

type Lock struct {
	consul *consuladapter.Session
	key    string
	value  []byte

	clock         clock.Clock
	retryInterval time.Duration

	logger lager.Logger

	metricEmiter metric.Metric
}

func NewLock(
	consul *consuladapter.Session,
	lockKey string,
	lockValue []byte,
	clock clock.Clock,
	retryInterval time.Duration,
	logger lager.Logger,
) Lock {
	return Lock{
		consul: consul,
		key:    lockKey,
		value:  lockValue,

		clock:         clock,
		retryInterval: retryInterval,

		logger: logger,

		metricEmiter: metric.Metric(lockKey),
	}
}

func (l Lock) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	logger := l.logger.Session("lock", lager.Data{"key": l.key, "value": string(l.value)})
	logger.Info("starting")

	defer func() {
		l.consul.Destroy()
		logger.Info("done")
	}()

	acquireErr := make(chan error, 1)

	acquire := func(session *consuladapter.Session) {
		logger.Info("acquiring-lock")
		acquireErr <- session.AcquireLock(l.key, l.value)
	}

	var c <-chan time.Time

	go acquire(l.consul)

	for {
		select {
		case sig := <-signals:
			logger.Info("shutting-down", lager.Data{"received-signal": sig})

			logger.Debug("releasing-lock")
			l.metricEmiter.Send(0)
			return nil
		case err := <-l.consul.Err():
			var data lager.Data
			if err != nil {
				data = lager.Data{"err": err.Error()}
			}

			if ready == nil {
				logger.Info("lost-lock", data)
				l.metricEmiter.Send(0)
				return ErrLockLost
			}

			logger.Info("consul-error", data)
			c = l.clock.NewTimer(l.retryInterval).C()
		case err := <-acquireErr:
			if err != nil {
				logger.Info("acquire-lock-failed", lager.Data{"err": err.Error()})
				l.metricEmiter.Send(0)
				c = l.clock.NewTimer(l.retryInterval).C()
				break
			}

			logger.Info("acquire-lock-succeeded")
			l.metricEmiter.Send(1)

			close(ready)
			ready = nil
			c = nil
			logger.Info("started")
		case <-c:
			logger.Info("retrying-acquiring-lock")

			newSession, err := l.consul.Recreate()
			if err != nil {
				c = l.clock.NewTimer(l.retryInterval).C()
			} else {
				l.consul = newSession
				c = nil
				go acquire(newSession)
			}
		}
	}
}
