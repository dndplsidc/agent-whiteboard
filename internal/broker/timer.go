package broker

import "time"

// TimerFactory creates disposable one-shot timers owned by a conversation
// actor. Timers are stopped and discarded rather than reset.
type TimerFactory interface {
	NewTimer(time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type RealTimerFactory struct{}

func (RealTimerFactory) NewTimer(duration time.Duration) Timer {
	return &realTimer{timer: time.NewTimer(duration)}
}

type realTimer struct{ timer *time.Timer }

func (timer *realTimer) C() <-chan time.Time {
	if timer == nil || timer.timer == nil {
		return nil
	}
	return timer.timer.C
}
func (timer *realTimer) Stop() bool {
	return timer != nil && timer.timer != nil && timer.timer.Stop()
}
