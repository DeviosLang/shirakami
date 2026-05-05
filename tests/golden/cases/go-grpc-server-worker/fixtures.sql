-- fixtures.sql for go-grpc-server-worker
-- grpc-go v1.57: serverWorker 重构 + handleSingleStream + RecvBufferPool
-- symbol_nodes: 受改动影响的核心符号

INSERT INTO symbol_nodes (id, repo, file_path, name, kind, start_line, end_line, signature, commit_hash) VALUES
  ('grpc-go:server.go:serverWorker#0',
   'grpc-go', 'server.go', 'serverWorker', 'method', 228, 234,
   '()', 'c7f9d4e'),
  ('grpc-go:server.go:handleSingleStream#1',
   'grpc-go', 'server.go', 'handleSingleStream', 'method', 235, 240,
   '(data serverWorkerData)', 'c7f9d4e'),
  ('grpc-go:server.go:initServerWorkers#0',
   'grpc-go', 'server.go', 'initServerWorkers', 'method', 241, 248,
   '()', 'c7f9d4e'),
  ('grpc-go:server.go:stopServerWorkers#0',
   'grpc-go', 'server.go', 'stopServerWorkers', 'method', 249, 252,
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
INSERT INTO symbol_edges (from_id, to_id, kind, confidence, commit_hash) VALUES
  ('grpc-go:server.go:Serve#1',
   'grpc-go:server.go:initServerWorkers#0',
   'CALLS', 1.0, 'c7f9d4e'),
  ('grpc-go:server.go:initServerWorkers#0',
   'grpc-go:server.go:serverWorker#0',
   'CALLS', 1.0, 'c7f9d4e'),
  ('grpc-go:server.go:serverWorker#0',
   'grpc-go:server.go:handleSingleStream#1',
   'CALLS', 1.0, 'c7f9d4e'),
  ('grpc-go:server.go:handleSingleStream#1',
   'grpc-go:server.go:serveStreams#1',
   'CALLS', 1.0, 'c7f9d4e');
