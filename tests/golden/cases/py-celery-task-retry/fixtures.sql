-- fixtures.sql for py-celery-task-retry
-- celery v5.x: Task.retry() 重构 + Task.apply_async() 拆分 _apply_async_impl
-- Python 函数以 'function' kind 存储

INSERT INTO symbol_nodes (id, repo, file_path, name, kind, start_line, end_line, signature, commit_hash) VALUES
  -- Task.retry: 修改的核心方法，@@ -648 处
  ('celery:celery/app/task.py:Task.retry#1',
   'celery', 'celery/app/task.py', 'Task.retry', 'method', 648, 695,
   '(self, args=None, kwargs=None, exc=None, throw=True, eta=None, countdown=None, max_retries=None, **options)',
   'celery-5x'),
  -- Task.apply_async: 第二个修改点，@@ -714 处
  ('celery:celery/app/task.py:Task.apply_async#1',
   'celery', 'celery/app/task.py', 'Task.apply_async', 'method', 714, 760,
   '(self, args=None, kwargs=None, task_id=None, producer=None, link=None, link_error=None, shadow=None, **options)',
   'celery-5x'),
  -- Task.signature_from_request: retry 内调用
  ('celery:celery/app/task.py:Task.signature_from_request#1',
   'celery', 'celery/app/task.py', 'Task.signature_from_request', 'method', 780, 800,
   '(self, request, args=None, kwargs=None, **extra)',
   'celery-5x'),
  -- Task._apply_async_impl: apply_async 委托的实现
  ('celery:celery/app/task.py:Task._apply_async_impl#1',
   'celery', 'celery/app/task.py', 'Task._apply_async_impl', 'method', 801, 840,
   '(self, args, kwargs, task_id=None, producer=None, link=None, link_error=None, shadow=None, **options)',
   'celery-5x'),
  -- Task.__call__: entry point (MQ consumer)
  ('celery:celery/app/task.py:Task.__call__#1',
   'celery', 'celery/app/task.py', 'Task.__call__', 'method', 600, 620,
   '(self, *args, **kwargs)',
   'celery-5x');

-- symbol_edges
INSERT INTO symbol_edges (id, source_id, target_id, type, file_path, line, confidence) VALUES
  -- retry → signature_from_request
  ('celery:edge:Task.retry->Task.signature_from_request',
   'celery:celery/app/task.py:Task.retry#1',
   'celery:celery/app/task.py:Task.signature_from_request#1',
   'CALLS', 'celery/app/task.py', 658, 1.0),
  -- retry → apply_async (via S.apply_async)
  ('celery:edge:Task.retry->Task.apply_async',
   'celery:celery/app/task.py:Task.retry#1',
   'celery:celery/app/task.py:Task.apply_async#1',
   'CALLS', 'celery/app/task.py', 665, 1.0),
  -- apply_async → _apply_async_impl
  ('celery:edge:Task.apply_async->Task._apply_async_impl',
   'celery:celery/app/task.py:Task.apply_async#1',
   'celery:celery/app/task.py:Task._apply_async_impl#1',
   'CALLS', 'celery/app/task.py', 745, 1.0),
  -- __call__ → retry (indirect: user code may call retry from within task)
  ('celery:edge:Task.__call__->Task.retry',
   'celery:celery/app/task.py:Task.__call__#1',
   'celery:celery/app/task.py:Task.retry#1',
   'CALLS', 'celery/app/task.py', 610, 0.8);
