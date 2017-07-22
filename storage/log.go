package storage

import (
	"github.com/gogo/protobuf/types"
	"github.com/pkg/errors"
	"github.com/xorlev/slogd/internal"
	pb "github.com/xorlev/slogd/proto"
	"go.uber.org/zap"
	"golang.org/x/net/context"
	"io/ioutil"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	MESSAGE_SIZE_LIMIT = 16 * 1024 * 1024
	SEGMENT_SIZE_LIMIT = 16 * 1024 * 1024
)

type LogEntryChannel chan *internal.TopicAndLog

type Log interface {
	Name() string
	LogChannel() LogEntryChannel
	Retrieve(context.Context, *LogFilter) ([]*pb.LogEntry, *Continuation, error)
	ContinueRetrieve(context.Context, *Continuation) ([]*pb.LogEntry, error)
	Append(context.Context, []*pb.LogEntry) ([]uint64, error)
	Length() uint64
	Close() error
}

type FileLog struct {
	Log
	sync.RWMutex

	logger              *zap.SugaredLogger
	basePath            string
	closed              bool
	topic               string
	messagesWithOffsets LogEntryChannel
	nextOffset          uint64
	segments            []logSegment
	currentSegment      logSegment
}

type LogFilter struct {
	StartOffset uint64
	MaxMessages uint32
	Timestamp   time.Time
}

func (lf *LogFilter) LogPassesFilter(entry *pb.LogEntry) (bool, error) {
	if !lf.Timestamp.IsZero() {
		ts, err := types.TimestampFromProto(entry.GetTimestamp())
		if err != nil {
			return false, err
		}

		return ts == lf.Timestamp || ts.After(lf.Timestamp), nil
	} else if lf.StartOffset > 0 {
		return entry.GetOffset() >= lf.StartOffset, nil
	}

	return true, nil
}

type Continuation struct {
	LastOffsetRead uint64
	FilePosition   uint64
	LogFilter      *LogFilter
}

func (fl *FileLog) Name() string {
	return fl.topic
}

func (fl *FileLog) LogChannel() LogEntryChannel {
	return fl.messagesWithOffsets
}

func (fl *FileLog) Retrieve(ctx context.Context, req *LogFilter) ([]*pb.LogEntry, *Continuation, error) {
	logEntries := make([]*pb.LogEntry, 0)
	scannedLogs := 0

	fl.RLock()
	segments := make([]logSegment, len(fl.segments))
	copy(segments, fl.segments)
	fl.RUnlock()

	for _, segment := range segments {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}

		messagesRead := uint32(len(logEntries))

		// TODO: add EndTimestamp?
		if segment.EndOffset() > req.StartOffset && messagesRead < req.MaxMessages {
			segmentLogs, scanned, _, err := segment.Retrieve(ctx, req, -1, req.MaxMessages-messagesRead)
			if err != nil {
				return nil, nil, errors.Wrap(err, "Error reading from segment")
			}

			logEntries = append(logEntries, segmentLogs...)
			scannedLogs += scanned
		}
	}

	fl.logger.Debugf("Scanned %d logs for %d returned messages",
		scannedLogs, len(logEntries))

	return logEntries, nil, nil
}

// func (fl *FileLog) ContinueRetrieve(ctx context.Context, continuation *Continuation) ([]*pb.LogEntry, error) {
// 	logEntries := make([]*pb.LogEntry, 0)
// 	scannedLogs := 0

// 	fl.RLock()
// 	segments := make([]logSegment, len(fl.segments))
// 	copy(segments, fl.segments)
// 	fl.RUnlock()

// 	nextOffset := continuation.LastOffsetRead + 1

// 	continuation
// 	for _, segment := range segments {
// 		messagesRead := uint32(len(logEntries))

// 		if segment.EndOffset() > nextOffset && messagesRead < req.MaxMessages {
// 			segmentLogs, scanned, pos, err := segment.Retrieve(ctx, nextOffset, continuation.FilePosition, req.MaxMessages-messagesRead)
// 			if err != nil {
// 				return nil, err
// 			}

// 			logEntries = append(logEntries, segmentLogs...)
// 			scannedLogs += scanned

// 			if len(segmentLogs) > 0
// 				lastLog := segmentLogs[len(segmentLogs) - 1]

// 		}
// 	}

// 	fl.logger.Debugf("Scanned %d logs for %d returned messages",
// 		scannedLogs, len(logEntries))

// 	return logEntries, nil
// }

func (fl *FileLog) Append(ctx context.Context, logs []*pb.LogEntry) ([]uint64, error) {
	fl.Lock()
	defer fl.Unlock()

	newOffsets := make([]uint64, len(logs))
	for i, log := range logs {
		if log != nil {
			fl.logger.Debugf("Appending: %+v", log)
			log.Offset = fl.nextOffset
			newOffsets[i] = fl.nextOffset
			fl.nextOffset += 1

			fl.currentSegment.Append(ctx, log)
		}
	}

	fl.currentSegment.Flush()

	// Roll log segment if necessary
	if err := fl.rollLogIfNecessary(); err != nil {
		return nil, errors.Wrap(err, "Error rolling log")
	}

	// Publish to registered stream
	for _, log := range logs {
		fl.messagesWithOffsets <- &internal.TopicAndLog{
			Topic:    fl.topic,
			LogEntry: log,
		}
	}

	fl.logger.Debug("Done with Append()")

	return newOffsets, nil
}

func (fl *FileLog) Length() uint64 {
	fl.RLock()
	defer fl.RUnlock()

	return fl.nextOffset
}

func (fl *FileLog) Close() error {
	fl.Lock()
	defer fl.Unlock()

	for _, segment := range fl.segments {
		if err := segment.Close(); err != nil {
			return err
		}
	}

	fl.closed = true

	return nil
}

func (fl *FileLog) rollLogIfNecessary() error {
	// Called from write lock
	// TODO config
	if fl.currentSegment.SizeBytes() >= SEGMENT_SIZE_LIMIT {
		newSegment, err := openSegment(fl.logger, fl.basePath, fl.nextOffset)
		if err != nil {
			return err
		}
		fl.currentSegment = newSegment

		fl.segments = append(fl.segments, fl.currentSegment)
	}

	return nil
}

func OpenLogs(logger *zap.SugaredLogger, directory string) (map[string]Log, error) {
	entries, err := ioutil.ReadDir(directory)
	if err != nil {
		return nil, err
	}

	logs := make(map[string]Log)

	for _, entry := range entries {
		if entry.IsDir() {
			// todo validate topic name
			topic := entry.Name()

			logs[topic], err = NewFileLog(logger, directory, topic)
			if err != nil {
				return nil, errors.Wrapf(err, "Error opening log: %s", topic)
			}
		}
	}

	return logs, nil
}

type ByNumericalFilename []os.FileInfo

func (nf ByNumericalFilename) Len() int      { return len(nf) }
func (nf ByNumericalFilename) Swap(i, j int) { nf[i], nf[j] = nf[j], nf[i] }
func (nf ByNumericalFilename) Less(i, j int) bool {

	// Use path names
	pathA := nf[i].Name()
	pathB := nf[j].Name()

	// Grab integer value of each filename by parsing the string and slicing off
	// the extension
	a, err1 := strconv.ParseInt(pathA[0:strings.LastIndex(pathA, ".")], 10, 64)
	b, err2 := strconv.ParseInt(pathB[0:strings.LastIndex(pathB, ".")], 10, 64)

	// If any were not numbers sort lexographically
	if err1 != nil || err2 != nil {
		return pathA < pathB
	}

	// Which integer is smaller?
	return a < b
}

func NewFileLog(logger *zap.SugaredLogger, directory string, topic string) (*FileLog, error) {
	basePath := path.Join(directory, topic)

	ctxLogger := logger.With(
		"topic", topic,
	)

	exists, err := exists(basePath)
	if err != nil {
		return nil, err
	}

	if !exists {
		if err := os.Mkdir(basePath, 0755); err != nil {
			return nil, err
		}
	}

	entries, err := ioutil.ReadDir(basePath)
	if err != nil {
		return nil, err
	}

	sort.Sort(ByNumericalFilename(entries))

	segments := make([]logSegment, 0)

	if len(entries) == 0 {
		// for each segment...
		fls, err := openSegment(logger, basePath, 0)
		if err != nil {
			return nil, errors.Wrap(err, "Error creating new segment")
		}

		segments = append(segments, fls)
	} else {
		for _, entry := range entries { // TODO TODO sort numerically!
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".log") {
				ctxLogger.Debugw("Opening segment",
					"filename", entry.Name(),
				)
				startOffset, err := strconv.ParseUint(strings.TrimSuffix(entry.Name(), ".log"), 10, 64)
				if err != nil {
					return nil, errors.Wrap(err, "Error parsing log filename")
				}

				fls, err := openSegment(ctxLogger, basePath, startOffset)
				if err != nil {
					return nil, errors.Wrapf(err, "Error opening segment: %s/%d.log", basePath, startOffset)
				}

				ctxLogger.Debugw("Opened segment",
					"filename", entry.Name(),
					"startOffset", fls.StartOffset(),
					"endOffset", fls.EndOffset())

				segments = append(segments, fls)
			}
		}
	}

	lastSegment := segments[len(segments)-1]
	nextOffset := lastSegment.EndOffset()

	fl := &FileLog{
		basePath:            basePath,
		topic:               topic,
		nextOffset:          nextOffset,
		messagesWithOffsets: make(LogEntryChannel),
		logger:              ctxLogger,
		segments:            segments,
		currentSegment:      lastSegment,
	}


	return fl, nil
}
