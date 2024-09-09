package asyncbufio

import (
	"bufio"
	"io"
	"time"
	"sync/atomic"
	"log"
)

// Writer provides asynchronous writing to an underlying io.Writer using buffered channels.
type Writer struct {
	writer        *bufio.Writer // Buffered writer: this does the writing
	flushNow      chan struct{} // Channel to signal the underlying writer to flush itself
	flushComplete chan struct{} // Channel to signal underlying writer flush is complete
	datachannel   chan []byte   // Channel to hold data before writing it
	flushInterval time.Duration // Interval for flushing the writer periodically
	bytesBuffered int64
	bytesWritten int64
}

// NewWriter creates a new Writer instance.
func NewWriter(w io.Writer, channelDepth int, flushInterval time.Duration) *Writer {
	aw := &Writer{
		writer:        bufio.NewWriter(w),
		datachannel:   make(chan []byte, channelDepth),
		flushNow:      make(chan struct{}),
		flushComplete: make(chan struct{}),
		flushInterval: flushInterval, // Set the flush interval
		bytesBuffered: 0,
		bytesWritten: 0,
	}
	go aw.writeLoop()
	return aw
}

// Write sends data to the Writer's channel, storing it for later writing.
func (aw *Writer) Write(p []byte) (int, error) {
	atomic.AddInt64(&aw.bytesBuffered, int64(len(p)))
	select {
	case aw.datachannel <- p:
		return len(p), nil
	default:
		return 0, io.ErrShortWrite // Return an error if channel is full
	}
}

// WriteString sends a string to the channel for later writing (with an annoying copy--sorry!)
func (aw *Writer) WriteString(s string) (int, error) {
	return aw.Write([]byte(s))
}

// Flush flushes any remaining data in the channel to the underlying writer.
// Blocks until the flush is complete.
func (aw *Writer) Flush() error {
	start := time.Now()
	bytesBeforeFlush := atomic.LoadInt64(&aw.bytesBuffered)
	aw.flushNow <- struct{}{}
	<-aw.flushComplete
	duration := time.Since(start)
	bytesAfterFlush := atomic.LoadInt64(&aw.bytesBuffered)
	if duration > 50 * time.Millisecond {
		log.Printf("AsyncBufio Flush took %v. Bytes before: %d, after: %d\n",
			duration, bytesBeforeFlush, bytesAfterFlush)
	}
	return nil
}

// Close closes the Writer, flushing remaining data and waiting for the writeLoop to finish.
// It will cause a panic to call Write(p) or Flush() after Close()--we don't
// test for that case.
func (aw *Writer) Close() {
	close(aw.flushNow) // Closing the flushNow channel signals the writeLoop to exit
	<-aw.flushComplete // Wait until writing is complete
}

// writeLoop is a goroutine that continuously moves data from the channel to the writer.
func (aw *Writer) writeLoop() {
	ticker := time.NewTicker(aw.flushInterval) // Ticker to flush periodically
	defer ticker.Stop()                        // Stop the ticker when the writeLoop exits

	for {
		select {
		case data := <-aw.datachannel:
			n, err := aw.writer.Write(data) // Write data from the channel to the writer
			if err != nil {
				log.Printf("Error writing to buffer %v\n", err)
			}
			atomic.AddInt64(&aw.bytesBuffered, -int64(n))
			atomic.AddInt64(&aw.bytesWritten, int64(n))
		case _, ok := <-aw.flushNow:
			aw.flush()
			// Signal whoever requested this that flushing is done
			aw.flushComplete <- struct{}{}
			if !ok {
				return
			}

		case <-ticker.C:
			aw.flush()
		}
	}
}

func (aw *Writer) flush() {
	// This loop empties the aw.datachannel channel before finally
	// calling the underlying writer's Flush() method
	for {
		select {
		case data := <-aw.datachannel:
			n, err := aw.writer.Write(data)
			if err != nil {
				log.Printf("ERror writing to buffer during flush: %v\n", err)
			}
			atomic.AddInt64(&aw.bytesWritten, int64(n))
			atomic.AddInt64(&aw.bytesBuffered, -int64(n))
		default:
			flushStart := time.Now()
			aw.writer.Flush()
			flushDuration := time.Since(flushStart)
			atomic.StoreInt64(&aw.bytesWritten, 0)
			//syncStart := time.Now()
			// aw.writer.file.Sync()
			//syncDuration := time.Since(syncStart)
			if flushDuration > 50*time.Millisecond {			
				log.Printf("Underlying writer Flush took %v. Bytes written: %d\n",
					flushDuration, atomic.LoadInt64(&aw.bytesWritten))
			}
			return
		}
	}
}
