package server

import (
	pb "github.com/xorlev/slogd/proto"
	storage "github.com/xorlev/slogd/storage"
	"go.uber.org/zap"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"sync"
	"sync/atomic"
)

type structuredLogServer struct {
	clientID uint64

	sync.RWMutex

	config *Config
	logger *zap.SugaredLogger
	// map from string to Log
	topics      map[string]storage.Log
	subscribers map[string]map[uint64]storage.LogEntryChannel
}

func (s *structuredLogServer) GetLogs(ctx context.Context, req *pb.GetLogsRequest) (*pb.GetLogsResponse, error) {
	//clientID := atomic.AddUint64(&s.clientID, 1)

	resp := &pb.GetLogsResponse{}
	s.logger.Debugw("Processing GetLogs call",
		"request", req,
	)

	s.RLock()
	topic, ok := s.topics[req.GetTopic()]
	s.RUnlock()

	if ok {
		filter := &storage.LogFilter{
			StartOffset: req.GetOffset(),
			MaxMessages: uint32(req.GetMaxMessages()),
		}
		logs, _, err := topic.Retrieve(ctx, filter)
		if err != nil {
			return nil, grpc.Errorf(codes.Internal, "Error retrieving logs: %v", err)
		}

		resp.Logs = logs

		return resp, nil
	} else {
		return nil, grpc.Errorf(codes.InvalidArgument, "Topic does not exist: %v", req.GetTopic())
	}
}

func (s *structuredLogServer) AppendLogs(ctx context.Context, req *pb.AppendRequest) (*pb.AppendResponse, error) {
	//clientID := atomic.AddUint64(&s.clientID, 1)

	resp := &pb.AppendResponse{}

	s.logger.Debugw("Processing AppendLogs call",
		"request", req,
	)

	s.Lock()
	topic, ok := s.topics[req.GetTopic()]
	if !ok {
		// TODO fix subscriber agg channel
		log, err := storage.NewFileLog(s.logger, s.config.DataDir, req.GetTopic())

		if err != nil {
			s.Unlock()
			return nil, grpc.Errorf(codes.Internal, "Error creating new topic: %v", err)
		}

		s.topics[req.GetTopic()] = log
		topic = log
	}
	s.Unlock()

	offsets, err := topic.Append(ctx, req.GetLogs())
	if err != nil {
		return nil, grpc.Errorf(codes.Internal, "Error appending logs: %v", err)
	}

	s.logger.Debug("Post-append")

	resp.Offsets = offsets

	return resp, nil
}

func (s *structuredLogServer) StreamLogs(req *pb.GetLogsRequest, stream pb.StructuredLog_StreamLogsServer) error {
	clientID := atomic.AddUint64(&s.clientID, 1)

	s.logger.Debugw("Processing StreamLogs call",
		"request", req,
	)

	// TODO initial GetLogs to replay to now(), then stream
	// page 100 messages at a time, streaming messages, maintain max offset
	// once page is <100 messages, register subscriber,
	// wait for subscriber to have message, check if offset is max+1, if so deliver and hand to for/select
	// otherwise do one last GetLogs before for/select
	// once a page is < 100 messages, start a consumer

	s.RLock()
	topic, ok := s.topics[req.GetTopic()]
	s.RUnlock()

	if ok {

		ch := s.addSubscriber(req, clientID)

		c := &cursor{
			ctx:            stream.Context(),
			lastOffset:     min(req.GetOffset(), topic.Length()),
			unreadMessages: ch,
		}

		if err := c.consume(topic, stream.SendMsg); err != nil {
			return err
		}
		for {
			select {
			case <-stream.Context().Done():
				s.logger.Infow("Stream finished.",
					"clientID", clientID,
				)

				s.removeSubscriber(req, clientID)

				// Done, remove
				return nil
			case <-ch:
				if err := c.consume(topic, stream.SendMsg); err != nil {
					return err
				}

				// 	response := &pb.GetLogsResponse{
				// 		Logs: []*pb.LogEntry{log.LogEntry},
				// 	}

				// 	if err := stream.SendMsg(response); err != nil {
				// 		s.logger.Errorw("Failed to send message",
				// 			"err", err,
				// 			"clientID", clientID,
				// 		)

				// 		s.removeSubscriber(req, clientID)
				// 		return err
				// 	}
			}
		}

		select {
		case <-ch:
			// ignore, we're just clearing the channel if it has a value
		default:

		}
	} else {
		return grpc.Errorf(codes.InvalidArgument, "Topic does not exist: %v", req.GetTopic())
	}

	return nil
}

// really, go?
func min(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func (s *StructuredLogServer) addSubscriber(req *pb.GetLogsRequest, clientID uint64) storage.LogEntryChannel {
	s.Lock()
	val, ok := s.subscribers[req.GetTopic()]

	ch := make(storage.LogEntryChannel)
	if ok {
		val[clientID] = ch
	} else {
		s.subscribers[req.GetTopic()] = make(map[uint64]storage.LogEntryChannel)
		s.subscribers[req.GetTopic()][clientID] = ch
	}
	s.Unlock()

	return ch
}

func (s *structuredLogServer) removeSubscriber(req *pb.GetLogsRequest, clientID uint64) {
	s.Lock()
	close(s.subscribers[req.GetTopic()][clientID])
	delete(s.subscribers[req.GetTopic()], clientID)
	s.Unlock()
}

func (s *structuredLogServer) pubsubStub(logChannel storage.LogEntryChannel) {
	s.logger.Info("Starting pubsub listener..")
	for {
		for log := range logChannel {
			s.logger.Debugf("Published log: %+v", log)

			s.RLock()
			val, ok := s.subscribers[log.Topic]
			if ok {
				// non-blocking w/ select?
				for _, subscriber := range val {
					select {
					case subscriber <- log:
						// Noop
					default:
						// Skip
					}
				}
			}
			s.RUnlock()
		}
	}
}

func NewRpcHandler(logger *zap.SugaredLogger, config *Config) (*structuredLogServer, error) {
	// Read all topics (folder in storage dir)
	// Spin off Goroutine for each topic
	// Start server, protect with RWLock

	logger.Infow("Data dir",
		"data_dir", config.DataDir,
	)

	topics, err := storage.OpenLogs(logger, config.DataDir)
	if err != nil {
		return nil, err
	}

	s := &structuredLogServer{
		config:      config,
		logger:      logger,
		topics:      topics,
		subscribers: make(map[string]map[uint64]storage.LogEntryChannel),
	}

	agg := make(storage.LogEntryChannel)
	for _, log := range topics {
		go func(c storage.LogEntryChannel) {
			for msg := range c {
				agg <- msg
			}
		}(log.LogChannel())
	}

	go s.pubsubStub(agg)

	return s, nil
}
