package dastard

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/davecgh/go-spew/spew"
)

// TriggerCounter is a per-channel struct that counts triggers over an interval of
// FrameIndex values and stores a slice of messages about the count. It does not
// send these messages anywhere; that's the job of the TriggerBroker.
// It takes advantage of the fact that TriggerBroker provides a synchronization point
// so several TriggerCounters can count triggers for all channels in sync.
// Counts triggers between the FrameIndex values of [lo, hi] to learn trigger rate.
type TriggerCounter struct {
	channelIndex int
	hi           FrameIndex // the highest FrameIndex for which we should count triggers
	lo           FrameIndex // count trigs starting at this FrameIndex (earlier are errors)
	hiTime       time.Time  // expected real-world time corresponding to hi
	countsSeen   int
	stepDuration time.Duration // how long each trigger counting step should last
	sampleRate   float64
	keyTime      time.Time  // the time of one recent correspondence between time and FrameIndex
	keyFrame     FrameIndex // keyFrame occured at keyTime to the best of our knowledge
	initialized  bool
	messages     []triggerCounterMessage
}

type triggerCounterMessage struct {
	hiTime     time.Time
	duration   time.Duration
	countsSeen int
}

// TriggerRateMessage is used to publish trigger rate info over zmq
type TriggerRateMessage struct {
	HiTime     time.Time
	Duration   time.Duration
	CountsSeen []int
}

// NewTriggerCounter returns a TriggerCounter
func NewTriggerCounter(channelIndex int, stepDuration time.Duration) TriggerCounter {
	return TriggerCounter{channelIndex: channelIndex, stepDuration: stepDuration, messages: make([]triggerCounterMessage, 0)}
}

// initialize initializes the counter by starting the trigger-count "integration period"
func (tc *TriggerCounter) initialize() {
	// Set hiTime (end of the integration period) to the first multiple of stepDuration after keyTime
	hiTime := tc.keyTime.Round(tc.stepDuration)
	if hiTime.Before(tc.keyTime) {
		hiTime = hiTime.Add(tc.stepDuration)
	}
	tc.hiTime = hiTime

	tc.hi = tc.keyFrame + FrameIndex(roundint(tc.sampleRate*tc.hiTime.Sub(tc.keyTime).Seconds()))
	tc.lo = tc.hi - FrameIndex(roundint(tc.sampleRate*tc.stepDuration.Seconds())) + 1
	tc.initialized = true
}

// messageAndReset appends a new triggerCounterMessage to our slice of them and
// reset to count triggers in the subsequent interval.
func (tc *TriggerCounter) messageAndReset() {
	// Generate a message
	message := triggerCounterMessage{hiTime: tc.hiTime, duration: tc.stepDuration, countsSeen: tc.countsSeen}
	tc.messages = append(tc.messages, message)

	// Reset counters and lo/hi times.
	tc.countsSeen = 0
	tc.hiTime = tc.hiTime.Add(tc.stepDuration)
	tc.lo = tc.hi + 1
	tc.hi = tc.keyFrame + FrameIndex(roundint(tc.sampleRate*tc.hiTime.Sub(tc.keyTime).Seconds()))
}

func (tc *TriggerCounter) observeTriggerList(tList *triggerList) error {
	// Update keyFrame and keyTime to have a relatively recent correspondence between
	// the real-world time and frame number.
	tc.keyFrame = tList.keyFrame
	tc.keyTime = tList.keyTime
	tc.sampleRate = tList.sampleRate
	if !tc.initialized {
		tc.initialize()
	}
	for _, frame := range tList.frames {
		if frame > tc.hi {
			tc.messageAndReset()
		}
		if frame > tc.hi {
			return fmt.Errorf("frame %v still higher than tc.hi=%v after reset (Δf=%d)", frame, tc.hi, frame-tc.hi)
		}
		if frame < tc.lo {
			return fmt.Errorf("observed count before lo=%v, frame=%v", tc.lo, frame)
		}
		tc.countsSeen++
	}
	if tList.lastFrameThatWillNeverTrigger > tc.hi {
		tc.messageAndReset()
	}
	return nil
}

// TriggerBroker communicates with DataChannel objects to allow them to operate independently
// yet still share group triggering information.
type TriggerBroker struct {
	nchannels       int
	sources         []map[int]bool
	PrimaryTrigs    chan triggerList
	SecondaryTrigs  []chan []FrameIndex
	latestPrimaries [][]FrameIndex
	triggerCounters []TriggerCounter
	sync.RWMutex
}

// NewTriggerBroker creates a new TriggerBroker object for nchan channels to share group triggers.
func NewTriggerBroker(nchan int) *TriggerBroker {
	broker := new(TriggerBroker)
	broker.nchannels = nchan
	broker.sources = make([]map[int]bool, nchan)
	for i := 0; i < nchan; i++ {
		broker.sources[i] = make(map[int]bool)
	}
	broker.PrimaryTrigs = make(chan triggerList, nchan)
	broker.SecondaryTrigs = make([]chan []FrameIndex, nchan)
	for i := 0; i < nchan; i++ {
		broker.SecondaryTrigs[i] = make(chan []FrameIndex, 1)
	}
	broker.latestPrimaries = make([][]FrameIndex, nchan)
	broker.triggerCounters = make([]TriggerCounter, nchan)
	for i := 0; i < nchan; i++ {
		triggerReportRate := time.Second // could be programmable in future
		broker.triggerCounters[i] = NewTriggerCounter(i, triggerReportRate)
	}
	return broker
}

// AddConnection connects source -> receiver for group triggers.
// It is safe to add connections that already exist.
func (broker *TriggerBroker) AddConnection(source, receiver int) error {
	if receiver < 0 || receiver >= broker.nchannels {
		return fmt.Errorf("Could not add channel %d as a group receiver (nchannels=%d)",
			receiver, broker.nchannels)
	}
	broker.Lock()
	broker.sources[receiver][source] = true
	broker.Unlock()
	return nil
}

// DeleteConnection disconnects source -> receiver for group triggers.
// It is safe to delete connections whether they exist or not.
func (broker *TriggerBroker) DeleteConnection(source, receiver int) error {
	if receiver < 0 || receiver >= broker.nchannels {
		return fmt.Errorf("Could not remove channel %d as a group receiver (nchannels=%d)",
			receiver, broker.nchannels)
	}
	broker.Lock()
	delete(broker.sources[receiver], source)
	broker.Unlock()
	return nil
}

// isConnected returns whether source->receiver is connected.
func (broker *TriggerBroker) isConnected(source, receiver int) bool {
	if receiver < 0 || receiver >= broker.nchannels {
		return false
	}
	broker.RLock()
	_, ok := broker.sources[receiver][source]
	broker.RUnlock()
	return ok
}

// Connections returns a set of all sources for the given receiver.
func (broker *TriggerBroker) Connections(receiver int) map[int]bool {
	if receiver < 0 || receiver >= broker.nchannels {
		return nil
	}
	broker.RLock()
	sources := broker.sources[receiver]
	broker.RUnlock()
	return sources
}

// FrameIdxSlice attaches the methods of sort.Interface to []FrameIndex, sorting in increasing order.
type FrameIdxSlice []FrameIndex

func (p FrameIdxSlice) Len() int           { return len(p) }
func (p FrameIdxSlice) Less(i, j int) bool { return p[i] < p[j] }
func (p FrameIdxSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// Run one pass of brokering trigger frame #s from sources to receivers.
// It runs in the pattern: get a message from each channel (about their triggered
// frame numbers), then send a message to each channel (about their secondary triggers).
// should be called in a goroutine
func (broker *TriggerBroker) Distribute() error {
	timeout := time.NewTimer(time.Second)

	// get data from all PrimaryTrigs channels
	for i := 0; i < broker.nchannels; i++ {
		select {
		case <-timeout.C:
			return fmt.Errorf("TriggerBroker.Distribute() timed out")
		case tlist := <-broker.PrimaryTrigs:
			broker.latestPrimaries[tlist.channelIndex] = tlist.frames
			err := broker.triggerCounters[tlist.channelIndex].observeTriggerList(&tlist)
			if err != nil {
				log.Printf("triggering assumptions broken!\n%v\n%v\n%v", err,
					spew.Sdump(tlist), spew.Sdump(broker.triggerCounters[tlist.channelIndex]))
			}
		}
	}

	// send reponse to all SecondaryTrigs channels
	broker.RLock()
	for idx, rxchan := range broker.SecondaryTrigs {
		sources := broker.Connections(idx)
		var trigs []FrameIndex
		if len(sources) > 0 {
			for source := range sources {
				trigs = append(trigs, broker.latestPrimaries[source]...)
			}
			sort.Sort(FrameIdxSlice(trigs))
		}
		rxchan <- trigs
	}
	broker.RUnlock()

	// generate combined trigger rate message
	var hiTime time.Time
	var duration time.Duration
	nMessages := len(broker.triggerCounters[0].messages)
	for j := 1; j < broker.nchannels; j++ {
		if len(broker.triggerCounters[j].messages) != nMessages {
			msg := fmt.Sprintf("triggerCounter[%d] has %d messages, want %d", j, len(broker.triggerCounters[j].messages), nMessages)
			panic(msg)
		}
	}
	for i := 0; i < nMessages; i++ {
		// It's a data race if we don't make a new slice for each message:
		countsSeen := make([]int, broker.nchannels)
		for j := 0; j < broker.nchannels; j++ {
			message := broker.triggerCounters[j].messages[i]
			if j == 0 { // first channel
				hiTime = message.hiTime
				duration = message.duration
			}
			if message.hiTime.Nanosecond() != hiTime.Nanosecond() || message.duration.Nanoseconds() != duration.Nanoseconds() {
				panic("trigger messages not in sync")
			}
			countsSeen[j] = message.countsSeen
		}
		clientMessageChan <- ClientUpdate{tag: "TRIGGERRATE", state: TriggerRateMessage{HiTime: hiTime, Duration: duration, CountsSeen: countsSeen}}
	}
	for j := 0; j < broker.nchannels; j++ {
		broker.triggerCounters[j].messages = make([]triggerCounterMessage, 0) // release all memory
	}
	return nil
}
