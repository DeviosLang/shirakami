-- fixtures.sql for go-grpc-server-worker
-- grpc-go v1.57: serverWorker 重构 + handleSingleStream + RecvBufferPool
-- symbol_nodes: 受改动影响的核心符号

INSERT INTO symbol_nodes (id, repo, file_path, name, kind, start_line, end_line, signature, commit_hash) VALUES
  ('grpc-go:server.go:serverWorker#0',
   'grpc-go', 'server.go', 'serverWorker', 'function', 228, 234,
   '()', 'c7f9d4e'),
  ('grpc-go:server.go:handleSingleStream#1',
   'grpc-go', 'server.go', 'handleSingleStream', 'function', 235, 240,
   '(data serverWorkerData)', 'c7f9d4e'),
  ('grpc-go:server.go:initServerWorkers#0',
   'grpc-go', 'server.go', 'initServerWorkers', 'function', 241, 248,
   '()', 'c7f9d4e'),
  ('grpc-go:server.go:stopServerWorkers#0',
   'grpc-go', 'server.go', 'stopServerWorkers', 'function', 249, 252,
   '()', 'c7f9d4e'),
  ('grpc-go:server.go:RecvBufferPool#1',
   'grpc-go', 'server.go', 'RecvBufferPool', 'function', 199, 204,
   '(bufferPool SharedBufferPool) ServerOption', 'c7f9d4e'),
  ('grpc-go:server.go:Serve#1',
   'grpc-go', 'server.go', 'Serve', 'method', 295, 420,
   '(lis net.Listener) error', 'c7f9d4e'),
  ('grpc-go:server.go:serveStreams#1',
   'grpc-go', 'server.go', 'serveStreams', 'method', 475, 530,
   '(st transport.ServerTransport)', 'c7f9d4e');

-- symbol_edges: 调用关系
INSERT INTO symbol_edges (id, source_id, target_id, type, file_path, line, confidence) VALUES
  ('grpc-go:edge:Serve->initServerWorkers',
   'grpc-go:server.go:Serve#1',
   'grpc-go:server.go:initServerWorkers#0',
   'CALLS', 'server.go', 310, 1.0),
  ('grpc-go:edge:initServerWorkers->serverWorker',
   'grpc-go:server.go:initServerWorkers#0',
   'grpc-go:server.go:serverWorker#0',
   'CALLS', 'server.go', 244, 1.0),
  ('grpc-go:edge:serverWorker->handleSingleStream',
   'grpc-go:server.go:serverWorker#0',
   'grpc-go:server.go:handleSingleStream#1',
   'CALLS', 'server.go', 231, 1.0),
  ('grpc-go:edge:handleSingleStream->serveStreams',
   'grpc-go:server.go:handleSingleStream#1',
   'grpc-go:server.go:serveStreams#1',
   'CALLS', 'server.go', 238, 1.0);
