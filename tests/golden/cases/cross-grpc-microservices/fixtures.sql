-- fixtures.sql for cross-grpc-microservices
-- GCP microservices-demo: checkoutservice → currencyservice gRPC 调用
-- 此 case 主要验证 cross_repo_calls 检测，Layer B 提供跨仓库符号索引

INSERT INTO symbol_nodes (id, repo, file_path, name, kind, start_line, end_line, signature, commit_hash) VALUES
  -- checkoutservice 侧
  ('checkoutservice:src/checkoutservice/main.go:checkoutService.PlaceOrder#2',
   'checkoutservice', 'src/checkoutservice/main.go',
   'checkoutService.PlaceOrder', 'method', 178, 243,
   '(ctx context.Context, req *pb.PlaceOrderRequest) (*pb.PlaceOrderResponse, error)',
   'a9f4e31'),
  ('checkoutservice:src/checkoutservice/main.go:checkoutService.convertCurrency#3',
   'checkoutservice', 'src/checkoutservice/main.go',
   'checkoutService.convertCurrency', 'method', 244, 257,
   '(ctx context.Context, from *money.Money, toCurrency string) (*money.Money, error)',
   'a9f4e31'),
  ('checkoutservice:src/checkoutservice/main.go:checkoutService.getCurrencySvcClient#0',
   'checkoutservice', 'src/checkoutservice/main.go',
   'checkoutService.getCurrencySvcClient', 'method', 258, 278,
   '() (pb.CurrencyServiceClient, error)',
   'a9f4e31'),
  -- currencyservice 侧（被调用方）
  ('currencyservice:main.go:CurrencyService.Convert#2',
   'currencyservice', 'main.go',
   'CurrencyService.Convert', 'method', 52, 98,
   '(ctx context.Context, req *pb.CurrencyConversionRequest) (*pb.Money, error)',
   'deadbeef');

-- symbol_edges: 调用关系（GRPC_CALLS 降为 CALLS，schema 仅支持 CALLS/IMPORTS/EXTENDS/IMPLEMENTS）
INSERT INTO symbol_edges (id, source_id, target_id, type, file_path, line, confidence) VALUES
  ('checkoutservice:edge:PlaceOrder->convertCurrency',
   'checkoutservice:src/checkoutservice/main.go:checkoutService.PlaceOrder#2',
   'checkoutservice:src/checkoutservice/main.go:checkoutService.convertCurrency#3',
   'CALLS', 'src/checkoutservice/main.go', 210, 1.0),
  ('checkoutservice:edge:convertCurrency->getCurrencySvcClient',
   'checkoutservice:src/checkoutservice/main.go:checkoutService.convertCurrency#3',
   'checkoutservice:src/checkoutservice/main.go:checkoutService.getCurrencySvcClient#0',
   'CALLS', 'src/checkoutservice/main.go', 250, 1.0),
  -- 跨仓库 gRPC 调用（以 CALLS 类型存储，confidence=0.95 表示 gRPC 推断）
  ('checkoutservice:edge:convertCurrency->CurrencyService.Convert',
   'checkoutservice:src/checkoutservice/main.go:checkoutService.convertCurrency#3',
   'currencyservice:main.go:CurrencyService.Convert#2',
   'CALLS', 'src/checkoutservice/main.go', 253, 0.95);
