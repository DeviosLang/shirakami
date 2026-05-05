-- fixtures.sql for go-gin-context-json
-- gin v1.9.1: Context.Render() + Context.JSON() + IndentedJSON() 改动

INSERT INTO symbol_nodes (id, repo, file_path, name, kind, start_line, end_line, signature, commit_hash) VALUES
  ('gin:context.go:Context.Render#2',
   'gin', 'context.go', 'Context.Render', 'method', 927, 944,
   '(code int, r render.Render)', 'd4b45d9'),
  ('gin:context.go:Context.JSON#2',
   'gin', 'context.go', 'Context.JSON', 'method', 979, 982,
   '(code int, obj any)', 'd4b45d9'),
  ('gin:context.go:Context.IndentedJSON#2',
   'gin', 'context.go', 'Context.IndentedJSON', 'method', 987, 990,
   '(code int, obj any)', 'd4b45d9'),
  ('gin:context.go:Context.Abort#0',
   'gin', 'context.go', 'Context.Abort', 'method', 148, 152,
   '()', 'd4b45d9'),
  ('gin:gin.go:Engine.ServeHTTP#2',
   'gin', 'gin.go', 'Engine.ServeHTTP', 'method', 348, 356,
   '(w http.ResponseWriter, req *http.Request)', 'd4b45d9'),
  ('gin:gin.go:Engine.handleHTTPRequest#1',
   'gin', 'gin.go', 'Engine.handleHTTPRequest', 'method', 380, 440,
   '(c *Context)', 'd4b45d9');

-- symbol_edges
INSERT INTO symbol_edges (from_id, to_id, kind, confidence, commit_hash) VALUES
  ('gin:context.go:Context.JSON#2',
   'gin:context.go:Context.Render#2',
   'CALLS', 1.0, 'd4b45d9'),
  ('gin:context.go:Context.IndentedJSON#2',
   'gin:context.go:Context.Render#2',
   'CALLS', 1.0, 'd4b45d9'),
  ('gin:context.go:Context.Render#2',
   'gin:context.go:Context.Abort#0',
   'CALLS', 0.9, 'd4b45d9'),
  ('gin:gin.go:Engine.ServeHTTP#2',
   'gin:gin.go:Engine.handleHTTPRequest#1',
   'CALLS', 1.0, 'd4b45d9');
