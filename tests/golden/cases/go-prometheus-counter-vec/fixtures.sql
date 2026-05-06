-- fixtures.sql for go-prometheus-counter-vec
-- prometheus/client_golang v1.14→v1.17: NewCounterVec 拆分 + CounterVecOpts + createdTs

INSERT INTO symbol_nodes (id, repo, file_path, name, kind, start_line, end_line, signature, commit_hash) VALUES
  ('client_golang:prometheus/counter.go:NewCounterVec#1',
   'client_golang', 'prometheus/counter.go', 'NewCounterVec', 'function', 57, 78,
   '(opts CounterVecOpts) *CounterVec', 'b9e7f12'),
  ('client_golang:prometheus/counter.go:counter.Write#2',
   'client_golang', 'prometheus/counter.go', 'counter.Write', 'method', 110, 117,
   '(out *dto.Metric, createdTs *timestamppb.Timestamp) error', 'b9e7f12'),
  ('client_golang:prometheus/counter.go:counter.createdTimestamp#0',
   'client_golang', 'prometheus/counter.go', 'counter.createdTimestamp', 'method', 118, 124,
   '() *timestamppb.Timestamp', 'b9e7f12'),
  ('client_golang:prometheus/vec.go:VariableLabelsFromConstrained#1',
   'client_golang', 'prometheus/vec.go', 'VariableLabelsFromConstrained', 'function', 166, 173,
   '(cl ConstrainedLabels) []string', 'b9e7f12'),
  ('client_golang:prometheus/counter.go:NewDesc#1',
   'client_golang', 'prometheus/counter.go', 'NewDesc', 'function', 20, 56,
   '(fqName, help string, variableLabels []string, constLabels Labels) *Desc', 'b9e7f12'),
  ('client_golang:prometheus/vec.go:NewMetricVec#2',
   'client_golang', 'prometheus/vec.go', 'NewMetricVec', 'function', 60, 90,
   '(desc *Desc, newMetric func(lvs ...string) Metric) *MetricVec', 'b9e7f12');

-- symbol_edges
INSERT INTO symbol_edges (id, source_id, target_id, type, file_path, line, confidence) VALUES
  ('client_golang:edge:NewCounterVec->NewDesc',
   'client_golang:prometheus/counter.go:NewCounterVec#1',
   'client_golang:prometheus/counter.go:NewDesc#1',
   'CALLS', 'prometheus/counter.go', 62, 1.0),
  ('client_golang:edge:NewCounterVec->NewMetricVec',
   'client_golang:prometheus/counter.go:NewCounterVec#1',
   'client_golang:prometheus/vec.go:NewMetricVec#2',
   'CALLS', 'prometheus/counter.go', 70, 1.0),
  ('client_golang:edge:NewCounterVec->VariableLabelsFromConstrained',
   'client_golang:prometheus/counter.go:NewCounterVec#1',
   'client_golang:prometheus/vec.go:VariableLabelsFromConstrained#1',
   'CALLS', 'prometheus/counter.go', 68, 1.0),
  ('client_golang:edge:counter.Write->counter.createdTimestamp',
   'client_golang:prometheus/counter.go:counter.Write#2',
   'client_golang:prometheus/counter.go:counter.createdTimestamp#0',
   'CALLS', 'prometheus/counter.go', 114, 1.0);
