// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fuse

import (
	"fmt"
	"log"
	"path"
	"runtime"
	"sync"

	"golang.org/x/net/context"

	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/internal/fusekernel"
	"github.com/jacobsa/fuse/internal/fuseshim"
)

// A connection to the fuse kernel process.
type Connection struct {
	debugLogger *log.Logger
	errorLogger *log.Logger
	wrapped     *fuseshim.Conn

	// The context from which all op contexts inherit.
	parentCtx context.Context

	// For logging purposes only.
	nextOpID uint32

	mu sync.Mutex

	// A map from fuse "unique" request ID (*not* the op ID for logging used
	// above) to a function that cancel's its associated context.
	//
	// GUARDED_BY(mu)
	cancelFuncs map[uint64]func()
}

// Responsibility for closing the wrapped connection is transferred to the
// result. You must call c.close() eventually.
//
// The loggers may be nil.
func newConnection(
	parentCtx context.Context,
	debugLogger *log.Logger,
	errorLogger *log.Logger,
	wrapped *fuseshim.Conn) (c *Connection, err error) {
	c = &Connection{
		debugLogger: debugLogger,
		errorLogger: errorLogger,
		wrapped:     wrapped,
		parentCtx:   parentCtx,
		cancelFuncs: make(map[uint64]func()),
	}

	return
}

// Log information for an operation with the given ID. calldepth is the depth
// to use when recovering file:line information with runtime.Caller.
func (c *Connection) debugLog(
	opID uint32,
	calldepth int,
	format string,
	v ...interface{}) {
	if c.debugLogger == nil {
		return
	}

	// Get file:line info.
	var file string
	var line int
	var ok bool

	_, file, line, ok = runtime.Caller(calldepth)
	if !ok {
		file = "???"
	}

	fileLine := fmt.Sprintf("%v:%v", path.Base(file), line)

	// Format the actual message to be printed.
	msg := fmt.Sprintf(
		"Op 0x%08x %24s] %v",
		opID,
		fileLine,
		fmt.Sprintf(format, v...))

	// Print it.
	c.debugLogger.Println(msg)
}

// LOCKS_EXCLUDED(c.mu)
func (c *Connection) recordCancelFunc(
	fuseID uint64,
	f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.cancelFuncs[fuseID]; ok {
		panic(fmt.Sprintf("Already have cancel func for request %v", fuseID))
	}

	c.cancelFuncs[fuseID] = f
}

// Set up state for an op that is about to be returned to the user, given its
// underlying fuse opcode and request ID.
//
// Return a context that should be used for the op.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) beginOp(
	opCode uint32,
	fuseID uint64) (ctx context.Context) {
	// Start with the parent context.
	ctx = c.parentCtx

	// Set up a cancellation function.
	//
	// Special case: On Darwin, osxfuse aggressively reuses "unique" request IDs.
	// This matters for Forget requests, which have no reply associated and
	// therefore have IDs that are immediately eligible for reuse. For these, we
	// should not record any state keyed on their ID.
	//
	// Cf. https://github.com/osxfuse/osxfuse/issues/208
	if opCode != fusekernel.OpForget {
		var cancel func()
		ctx, cancel = context.WithCancel(ctx)
		c.recordCancelFunc(fuseID, cancel)
	}

	return
}

// Clean up all state associated with an op to which the user has responded,
// given its underlying fuse opcode and request ID. This must be called before
// a response is sent to the kernel, to avoid a race where the request's ID
// might be reused by osxfuse.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) finishOp(
	opCode uint32,
	fuseID uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Even though the op is finished, context.WithCancel requires us to arrange
	// for the cancellation function to be invoked. We also must remove it from
	// our map.
	//
	// Special case: we don't do this for Forget requests. See the note in
	// beginOp above.
	if opCode != fusekernel.OpForget {
		cancel, ok := c.cancelFuncs[fuseID]
		if !ok {
			panic(fmt.Sprintf("Unknown request ID in finishOp: %v", fuseID))
		}

		cancel()
		delete(c.cancelFuncs, fuseID)
	}
}

// LOCKS_EXCLUDED(c.mu)
func (c *Connection) handleInterrupt(fuseID uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// NOTE(jacobsa): fuse.txt in the Linux kernel documentation
	// (https://goo.gl/H55Dnr) defines the kernel <-> userspace protocol for
	// interrupts.
	//
	// In particular, my reading of it is that an interrupt request cannot be
	// delivered to userspace before the original request. The part about the
	// race and EAGAIN appears to be aimed at userspace programs that
	// concurrently process requests (cf. http://goo.gl/BES2rs).
	//
	// So in this method if we can't find the ID to be interrupted, it means that
	// the request has already been replied to.
	//
	// Cf. https://github.com/osxfuse/osxfuse/issues/208
	// Cf. http://comments.gmane.org/gmane.comp.file-systems.fuse.devel/14675
	cancel, ok := c.cancelFuncs[fuseID]
	if !ok {
		return
	}

	cancel()
}

// Read the next op from the kernel process. Return io.EOF if the kernel has
// closed the connection.
//
// This function delivers ops in exactly the order they are received from
// /dev/fuse. It must not be called multiple times concurrently.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) ReadOp() (op fuseops.Op, err error) {
	// Keep going until we find a request we know how to convert.
	for {
		// Read the next message from the fuseshim connection.
		var m *fuseshim.Message
		m, err = c.wrapped.ReadMessage()
		if err != nil {
			return
		}

		// Choose an ID for this operation for the purposes of logging.
		opID := c.nextOpID
		c.nextOpID++

		// Set up op dependencies.
		opCtx := c.beginOp(m.Hdr.Opcode, m.Hdr.Unique)

		var debugLogForOp func(int, string, ...interface{})
		if c.debugLogger != nil {
			debugLogForOp = func(calldepth int, format string, v ...interface{}) {
				c.debugLog(opID, calldepth+1, format, v...)
			}
		}

		sendReply := func(
			op fuseops.Op,
			fuseID uint64,
			replyMsg []byte,
			opErr error) (err error) {
			// Make sure we destroy the message, as required by
			// fuseshim.Connection.ReadMessage.
			defer m.Destroy()

			// Clean up state for this op.
			c.finishOp(m.Hdr.Opcode, m.Hdr.Unique)

			// Debug logging
			if c.debugLogger != nil {
				if opErr == nil {
					op.Logf("-> OK: %s", op.DebugString())
				} else {
					op.Logf("-> error: %v", opErr)
				}
			}

			// Error logging
			if opErr != nil && c.errorLogger != nil {
				c.errorLogger.Printf("(%s) error: %v", op.ShortDesc(), opErr)
			}

			// Send the reply to the kernel.
			err = c.wrapped.WriteToKernel(replyMsg)
			if err != nil {
				err = fmt.Errorf("WriteToKernel: %v", err)
				return
			}

			return
		}

		// Convert the message to an Op.
		op, err = fuseops.Convert(
			opCtx,
			m,
			c.wrapped.Protocol(),
			debugLogForOp,
			c.errorLogger,
			sendReply)

		if err != nil {
			err = fmt.Errorf("fuseops.Convert: %v", err)
			return
		}

		// Log the receipt of the operation.
		c.debugLog(opID, 1, "<- %v", op.ShortDesc())

		// Special case: responding to statfs is required to make mounting work on
		// OS X. We don't currently expose the capability for the file system to
		// intercept this.
		if _, ok := op.(*fuseops.InternalStatFSOp); ok {
			op.Respond(nil)
			continue
		}

		// Special case: handle interrupt requests.
		if interruptOp, ok := op.(*fuseops.InternalInterruptOp); ok {
			c.handleInterrupt(interruptOp.FuseID)
			continue
		}

		return
	}
}

func (c *Connection) waitForReady() (err error) {
	<-c.wrapped.Ready
	err = c.wrapped.MountError
	return
}

// Close the connection. Must not be called until operations that were read
// from the connection have been responded to.
func (c *Connection) close() (err error) {
	err = c.wrapped.Close()
	return
}
