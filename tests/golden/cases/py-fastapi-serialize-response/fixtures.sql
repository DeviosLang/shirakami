-- fixtures.sql for py-fastapi-serialize-response
-- FastAPI: serialize_response() + run_endpoint_function() 参数扩展
-- Python 函数以 'function' kind 存储

INSERT INTO symbol_nodes (id, repo, file_path, name, kind, start_line, end_line, signature, commit_hash) VALUES
  -- serialize_response: 修改的核心函数，@@ -58 处
  ('fastapi:fastapi/routing.py:serialize_response#1',
   'fastapi', 'fastapi/routing.py', 'serialize_response', 'function', 58, 100,
   '(field=None, response_content=None, include=None, exclude=None, include_none=True, by_alias=True, exclude_unset=False, exclude_defaults=False, exclude_none=False)',
   'fastapi-0.9x'),
  -- run_endpoint_function: 第二个修改点，@@ -122 处
  ('fastapi:fastapi/routing.py:run_endpoint_function#1',
   'fastapi', 'fastapi/routing.py', 'run_endpoint_function', 'function', 122, 160,
   '(*, dependant, values, is_coroutine)',
   'fastapi-0.9x'),
  -- get_request_handler: 调用 serialize_response 的上层入口
  ('fastapi:fastapi/routing.py:get_request_handler#1',
   'fastapi', 'fastapi/routing.py', 'get_request_handler', 'function', 170, 240,
   '(dependant, body_field=None, status_code=200, response_class=None, response_field=None, response_model_include=None, response_model_exclude=None, response_model_by_alias=True)',
   'fastapi-0.9x'),
  -- _prepare_response_content: serialize_response 内部调用
  ('fastapi:fastapi/routing.py:_prepare_response_content#1',
   'fastapi', 'fastapi/routing.py', '_prepare_response_content', 'function', 40, 57,
   '(res, exclude_unset=False, exclude_defaults=False, exclude_none=False)',
   'fastapi-0.9x');

-- symbol_edges
INSERT INTO symbol_edges (id, source_id, target_id, type, file_path, line, confidence) VALUES
  -- get_request_handler → run_endpoint_function
  ('fastapi:edge:get_request_handler->run_endpoint_function',
   'fastapi:fastapi/routing.py:get_request_handler#1',
   'fastapi:fastapi/routing.py:run_endpoint_function#1',
   'CALLS', 'fastapi/routing.py', 205, 1.0),
  -- get_request_handler → serialize_response
  ('fastapi:edge:get_request_handler->serialize_response',
   'fastapi:fastapi/routing.py:get_request_handler#1',
   'fastapi:fastapi/routing.py:serialize_response#1',
   'CALLS', 'fastapi/routing.py', 215, 1.0),
  -- serialize_response → _prepare_response_content
  ('fastapi:edge:serialize_response->_prepare_response_content',
   'fastapi:fastapi/routing.py:serialize_response#1',
   'fastapi:fastapi/routing.py:_prepare_response_content#1',
   'CALLS', 'fastapi/routing.py', 75, 1.0);
