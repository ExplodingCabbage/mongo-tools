package archive

import (
	"fmt"
	"github.com/mongodb/mongo-tools/common/db"
	"github.com/mongodb/mongo-tools/common/intents"
	"github.com/mongodb/mongo-tools/common/log"
	"gopkg.in/mgo.v2/bson"
	"hash"
	"hash/crc64"
	"io"
	"reflect"
)

// bufferSize enables or disables the MuxIn buffering
// TODO: remove this constant and the non-buffered MuxIn implementations
const bufferWrites = true
const bufferSize = db.MaxBSONSize

// Multiplexer is what one uses to create interleaved intents in an archive
type Multiplexer struct {
	Out       io.WriteCloser
	Control   chan *MuxIn
	Completed chan error
	// ins and selectCases are correlating slices
	ins              []*MuxIn
	selectCases      []reflect.SelectCase
	currentNamespace string
}

// NewMultiplexer creates a Multiplexer and populates its Control/Completed chans
func NewMultiplexer(out io.WriteCloser) *Multiplexer {
	mux := &Multiplexer{
		Out:       out,
		Control:   make(chan *MuxIn),
		Completed: make(chan error),
		ins: []*MuxIn{
			nil, // There is no MuxIn for the Control case
		},
	}
	mux.selectCases = []reflect.SelectCase{
		reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(mux.Control),
			Send: reflect.Value{},
		},
	}
	return mux
}

// Run multiplexes until it receives an EOF on its Control chan.
func (mux *Multiplexer) Run() {
	var err error
	for {
		index, value, notEOF := reflect.Select(mux.selectCases)
		EOF := !notEOF
		if index == 0 { //Control index
			if EOF {
				log.Logf(log.DebugLow, "Mux finish")
				mux.Out.Close()
				if len(mux.selectCases) != 1 {
					mux.Completed <- fmt.Errorf("Mux ending but selectCases still open %v\n",
						len(mux.selectCases))
					return
				}
				mux.Completed <- nil
				return
			}
			muxIn, ok := value.Interface().(*MuxIn)
			if !ok {
				mux.Completed <- fmt.Errorf("non MuxIn received on Control chan") // one for the MuxIn.Open
				return
			}
			log.Logf(log.DebugLow, "Mux open namespace %v", muxIn.Intent.Namespace())
			mux.selectCases = append(mux.selectCases, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: reflect.ValueOf(muxIn.writeChan),
				Send: reflect.Value{},
			})
			mux.ins = append(mux.ins, muxIn)
		} else {
			if EOF {
				err = mux.formatEOF(index, mux.ins[index])
				if err != nil {
					mux.Completed <- err
					return
				}
				log.Logf(log.DebugLow, "Mux close namespace %v", mux.ins[index].Intent.Namespace())
				mux.currentNamespace = ""
				mux.selectCases = append(mux.selectCases[:index], mux.selectCases[index+1:]...)
				mux.ins = append(mux.ins[:index], mux.ins[index+1:]...)
			} else {
				bsonBytes, ok := value.Interface().([]byte)
				if !ok {
					mux.Completed <- fmt.Errorf("multiplexer received a value that wasn't a []byte")
					return
				}
				mux.ins[index].hash.Write(bsonBytes)
				err = mux.formatBody(mux.ins[index], bsonBytes)
				if err != nil {
					mux.Completed <- err
					return
				}
			}
		}
	}
}

// formatBody writes the BSON in to the archive, potentially writing a new header
// if the document belongs to a different namespace from the last header.
func (mux *Multiplexer) formatBody(in *MuxIn, bsonBytes []byte) error {
	var err error
	if in.Intent.Namespace() != mux.currentNamespace {
		// Handle the change of which DB/Collection we're writing docs for
		// If mux.currentNamespace then we need to terminate the current block
		if mux.currentNamespace != "" {
			l, err := mux.Out.Write(terminatorBytes)
			if err != nil {
				return err
			}
			if l != len(terminatorBytes) {
				return io.ErrShortWrite
			}
		}
		header, err := bson.Marshal(NamespaceHeader{
			Database:   in.Intent.DB,
			Collection: in.Intent.C,
		})
		if err != nil {
			return err
		}
		l, err := mux.Out.Write(header)
		if err != nil {
			return err
		}
		if l != len(header) {
			return io.ErrShortWrite
		}
	}
	mux.currentNamespace = in.Intent.Namespace()
	length, err := mux.Out.Write(bsonBytes)
	if err != nil {
		return err
	}
	in.writeLenChan <- length
	return nil
}

// formatEOF writes the EOF header in to the archive
func (mux *Multiplexer) formatEOF(index int, in *MuxIn) error {
	var err error
	if mux.currentNamespace != "" {
		l, err := mux.Out.Write(terminatorBytes)
		if err != nil {
			return err
		}
		if l != len(terminatorBytes) {
			return io.ErrShortWrite
		}
	}
	eofHeader, err := bson.Marshal(NamespaceHeader{
		Database:   in.Intent.DB,
		Collection: in.Intent.C,
		EOF:        true,
		CRC:        int64(in.hash.Sum64()),
	})
	if err != nil {
		return err
	}
	l, err := mux.Out.Write(eofHeader)
	if err != nil {
		return err
	}
	if l != len(eofHeader) {
		return io.ErrShortWrite
	}
	l, err = mux.Out.Write(terminatorBytes)
	if err != nil {
		return err
	}
	if l != len(terminatorBytes) {
		return io.ErrShortWrite
	}
	return nil
}

// MuxIn is an implementation of the intents.file interface.
// They live in the intents, and are potentially owned by different threads than
// the thread owning the Multiplexer.
// They are out the intents write data to the multiplexer
type MuxIn struct {
	writeChan    chan []byte
	writeLenChan chan int
	buf          []byte
	hash         hash.Hash64
	Intent       *intents.Intent
	Mux          *Multiplexer
}

// Read does nothing for MuxIns
func (muxIn *MuxIn) Read([]byte) (int, error) {
	return 0, nil
}

// Close closes the chans in the MuxIn.
// Ultimately the multiplexer will detect that they are closed and cause a
// formatEOF to occur.
func (muxIn *MuxIn) Close() error {
	// the mux side of this gets closed in the mux when it gets an eof on the read
	log.Logf(log.DebugHigh, "MuxIn close %v", muxIn.Intent.Namespace())
	if bufferWrites {
		muxIn.writeChan <- muxIn.buf
		length := <-muxIn.writeLenChan
		if length != len(muxIn.buf) {
			return io.ErrShortWrite
		}
		muxIn.buf = nil
	}
	close(muxIn.writeChan)
	close(muxIn.writeLenChan)
	return nil
}

// Open is implemented in Mux.open, but in short, it creates chans and a select case
// and adds the SelectCase and the MuxIn in to the Multiplexer.
func (muxIn *MuxIn) Open() error {
	log.Logf(log.DebugHigh, "MuxIn open %v", muxIn.Intent.Namespace())
	muxIn.writeChan = make(chan []byte)
	muxIn.writeLenChan = make(chan int)
	muxIn.buf = make([]byte, 0, bufferSize)
	muxIn.hash = crc64.New(crc64.MakeTable(crc64.ECMA))
	if bufferWrites {
		muxIn.buf = make([]byte, 0, db.MaxBSONSize)
	}
	muxIn.Mux.Control <- muxIn
	return nil
}

// Write hands a buffer to the Multiplexer and receives a written length from the multiplexer
// after the length is received, the buffer is free to be reused.
func (muxIn *MuxIn) Write(buf []byte) (int, error) {
	size := int(
		(uint32(buf[0]) << 0) |
			(uint32(buf[1]) << 8) |
			(uint32(buf[2]) << 16) |
			(uint32(buf[3]) << 24),
	)
	// TODO remove these checks, they're for debugging
	if len(buf) < size {
		panic(fmt.Errorf("corrupt bson in MuxIn.Write (size %v/%v)", size, len(buf)))
	}
	if buf[size-1] != 0 {
		panic(fmt.Errorf("corrupt bson in MuxIn.Write bson has no-zero terminator %v, (size %v/%v)", buf[size-1], size, len(buf)))
	}
	if bufferWrites {
		if len(muxIn.buf)+len(buf) > cap(muxIn.buf) {
			muxIn.writeChan <- muxIn.buf
			length := <-muxIn.writeLenChan
			if length != len(muxIn.buf) {
				return 0, io.ErrShortWrite
			}
			muxIn.buf = muxIn.buf[:0]
		}
		muxIn.buf = append(muxIn.buf, buf...)
	} else {
		muxIn.writeChan <- buf
		length := <-muxIn.writeLenChan
		if length != len(buf) {
			return 0, io.ErrShortWrite
		}
	}
	return len(buf), nil
}
