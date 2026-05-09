package tool

import (
	"strings"
	"testing"
)

func TestParseDiffHunks_SingleFile(t *testing.T) {
	diff := `diff --git a/service/payment.go b/service/payment.go
index abc1234..def5678 100644
--- a/service/payment.go
+++ b/service/payment.go
@@ -42,6 +42,10 @@ func (s *PaymentService) ProcessPayment(ctx context.Context, req *PayReq) error
 	if req.Amount <= 0 {
 		return ErrInvalidAmount
 	}
+	// Add retry logic
+	for i := 0; i < s.maxRetries; i++ {
+		err := s.gateway.Charge(ctx, req)
+		if err == nil {
 			return nil
 		}
 	}
`

	hunks := ParseDiffHunks(diff)

	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d: %+v", len(hunks), hunks)
	}

	h := hunks[0]
	if h.File != "service/payment.go" {
		t.Errorf("file = %q, want %q", h.File, "service/payment.go")
	}
	if h.StartLine != 45 {
		t.Errorf("start_line = %d, want 45", h.StartLine)
	}
	if h.EndLine != 48 {
		t.Errorf("end_line = %d, want 48", h.EndLine)
	}
}

func TestParseDiffHunks_MultipleFiles(t *testing.T) {
	diff := `diff --git a/handler/payment.go b/handler/payment.go
--- a/handler/payment.go
+++ b/handler/payment.go
@@ -10,3 +10,5 @@ func HandlePayment(w http.ResponseWriter, r *http.Request) {
 	svc := NewPaymentService()
+	svc.SetTimeout(30 * time.Second)
+	svc.SetMaxRetries(3)
 	resp, err := svc.ProcessPayment(r.Context(), req)
diff --git a/config/config.go b/config/config.go
--- a/config/config.go
+++ b/config/config.go
@@ -5,2 +5,4 @@ type Config struct {
 	DBHost string
+	PaymentTimeout int
+	PaymentRetries int
 }
`

	hunks := ParseDiffHunks(diff)

	if len(hunks) != 2 {
		t.Fatalf("expected 2 hunks, got %d: %+v", len(hunks), hunks)
	}

	// First hunk: handler/payment.go lines 11-12
	if hunks[0].File != "handler/payment.go" {
		t.Errorf("hunk[0].file = %q, want %q", hunks[0].File, "handler/payment.go")
	}
	if hunks[0].StartLine != 11 {
		t.Errorf("hunk[0].start = %d, want 11", hunks[0].StartLine)
	}
	if hunks[0].EndLine != 12 {
		t.Errorf("hunk[0].end = %d, want 12", hunks[0].EndLine)
	}

	// Second hunk: config/config.go lines 6-7
	if hunks[1].File != "config/config.go" {
		t.Errorf("hunk[1].file = %q, want %q", hunks[1].File, "config/config.go")
	}
	if hunks[1].StartLine != 6 {
		t.Errorf("hunk[1].start = %d, want 6", hunks[1].StartLine)
	}
	if hunks[1].EndLine != 7 {
		t.Errorf("hunk[1].end = %d, want 7", hunks[1].EndLine)
	}
}

func TestParseDiffHunks_DeletedFile(t *testing.T) {
	diff := `diff --git a/old.go b/old.go
deleted file mode 100644
--- a/old.go
+++ /dev/null
@@ -1,5 +0,0 @@
-package main
-
-func main() {
-	fmt.Println("hello")
-}
`

	hunks := ParseDiffHunks(diff)

	if len(hunks) != 0 {
		t.Fatalf("expected 0 hunks for deleted file, got %d: %+v", len(hunks), hunks)
	}
}

func TestParseDiffHunks_MultipleHunksInOneFile(t *testing.T) {
	diff := `diff --git a/service/order.go b/service/order.go
--- a/service/order.go
+++ b/service/order.go
@@ -10,3 +10,4 @@ func (s *OrderService) Create(ctx context.Context) error {
 	order := &Order{}
+	order.Status = "pending"
 	return s.repo.Save(ctx, order)
@@ -50,3 +51,4 @@ func (s *OrderService) Cancel(ctx context.Context, id string) error {
 	order.Status = "cancelled"
+	order.CancelledAt = time.Now()
 	return s.repo.Save(ctx, order)
`

	hunks := ParseDiffHunks(diff)

	if len(hunks) != 2 {
		t.Fatalf("expected 2 hunks, got %d: %+v", len(hunks), hunks)
	}

	if hunks[0].StartLine != 11 || hunks[0].EndLine != 11 {
		t.Errorf("hunk[0] = lines %d-%d, want 11-11", hunks[0].StartLine, hunks[0].EndLine)
	}
	if hunks[1].StartLine != 52 || hunks[1].EndLine != 52 {
		t.Errorf("hunk[1] = lines %d-%d, want 52-52", hunks[1].StartLine, hunks[1].EndLine)
	}
}

func TestParseDiffHunks_EmptyDiff(t *testing.T) {
	hunks := ParseDiffHunks("")
	if len(hunks) != 0 {
		t.Fatalf("expected 0 hunks for empty diff, got %d", len(hunks))
	}
}

// ── 改进 4: RawLines ──────────────────────────────────────────────────────────

func TestParseDiffHunks_RawLines_Basic(t *testing.T) {
	diff := `diff --git a/service/order.go b/service/order.go
--- a/service/order.go
+++ b/service/order.go
@@ -10,3 +10,5 @@ func (s *OrderService) Create(ctx context.Context) error {
 	order := &Order{}
+	order.Status = "pending"
+	order.CreatedAt = time.Now()
 	return s.repo.Save(ctx, order)
`
	hunks := ParseDiffHunks(diff)
	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(hunks))
	}
	h := hunks[0]
	if len(h.RawLines) != 2 {
		t.Fatalf("expected 2 raw lines, got %d: %v", len(h.RawLines), h.RawLines)
	}
	if !strings.HasPrefix(h.RawLines[0], "+") {
		t.Errorf("RawLines[0] should start with '+', got %q", h.RawLines[0])
	}
	if !strings.Contains(h.RawLines[0], `"pending"`) {
		t.Errorf("RawLines[0] should contain added content, got %q", h.RawLines[0])
	}
}

func TestParseDiffHunks_RawLines_IncludesRemovedLines(t *testing.T) {
	// Removed lines that appear AFTER the first + line (interleaved) are captured in RawLines.
	// In this diff the + appears before the -, so inChange is true when we see -.
	diff := `diff --git a/svc/payment.go b/svc/payment.go
--- a/svc/payment.go
+++ b/svc/payment.go
@@ -5,4 +5,4 @@ func Pay() {
+	timeout := 30
-	timeout := 10
 	doCharge(timeout)
`
	hunks := ParseDiffHunks(diff)
	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(hunks))
	}
	h := hunks[0]
	// Both the + line and the interleaved - line should appear in RawLines.
	found := map[string]bool{"-": false, "+": false}
	for _, l := range h.RawLines {
		if strings.HasPrefix(l, "-") {
			found["-"] = true
		}
		if strings.HasPrefix(l, "+") {
			found["+"] = true
		}
	}
	if !found["-"] {
		t.Errorf("RawLines should include the removed (-) line (interleaved), got: %v", h.RawLines)
	}
	if !found["+"] {
		t.Errorf("RawLines should include the added (+) line, got: %v", h.RawLines)
	}
}

func TestParseDiffHunks_RawLines_Cap40(t *testing.T) {
	// Build a diff with 60 added lines.
	var sb strings.Builder
	sb.WriteString("diff --git a/big.go b/big.go\n")
	sb.WriteString("--- a/big.go\n")
	sb.WriteString("+++ b/big.go\n")
	sb.WriteString("@@ -1,0 +1,60 @@ func Big() {\n")
	for i := 0; i < 60; i++ {
		sb.WriteString("+\tline := \"something\"\n")
	}

	hunks := ParseDiffHunks(sb.String())
	if len(hunks) == 0 {
		t.Fatal("expected at least 1 hunk")
	}
	// All added lines form one contiguous block → should be capped at 40.
	if len(hunks[0].RawLines) > 40 {
		t.Errorf("RawLines should be capped at 40, got %d", len(hunks[0].RawLines))
	}
}

// ── 改进 5: GlobalVar ─────────────────────────────────────────────────────────

func TestParseDiffHunks_GlobalVar_Python(t *testing.T) {
	// Module-level constant change — FuncContext is empty when the @@ context is
	// a variable assignment or empty.
	diff := `diff --git a/config.py b/config.py
--- a/config.py
+++ b/config.py
@@ -1,2 +1,2 @@
-TIMEOUT = 10
+TIMEOUT = 30
 DB_HOST = "localhost"
`
	hunks := ParseDiffHunks(diff)
	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(hunks))
	}
	// The hunk has no FuncContext (bare top-level change).
	// GlobalVar should be detected from the added line "+TIMEOUT = 30".
	h := hunks[0]
	if h.GlobalVar != "TIMEOUT" {
		t.Errorf("GlobalVar = %q, want %q", h.GlobalVar, "TIMEOUT")
	}
}

func TestParseDiffHunks_GlobalVar_Go(t *testing.T) {
	diff := `diff --git a/limits.go b/limits.go
--- a/limits.go
+++ b/limits.go
@@ -3,2 +3,2 @@
-var MAX_RETRIES = 3
+var MAX_RETRIES = 5
`
	hunks := ParseDiffHunks(diff)
	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(hunks))
	}
	h := hunks[0]
	if h.GlobalVar != "MAX_RETRIES" {
		t.Errorf("GlobalVar = %q, want %q", h.GlobalVar, "MAX_RETRIES")
	}
}

func TestParseDiffHunks_GlobalVar_GoConst(t *testing.T) {
	diff := `diff --git a/constants.go b/constants.go
--- a/constants.go
+++ b/constants.go
@@ -1,2 +1,2 @@
-const DEFAULT_POOL_SIZE = 10
+const DEFAULT_POOL_SIZE = 20
`
	hunks := ParseDiffHunks(diff)
	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(hunks))
	}
	h := hunks[0]
	if h.GlobalVar != "DEFAULT_POOL_SIZE" {
		t.Errorf("GlobalVar = %q, want %q", h.GlobalVar, "DEFAULT_POOL_SIZE")
	}
}

func TestParseDiffHunks_GlobalVar_NotInsideFunc(t *testing.T) {
	// When the hunk context IS a function, GlobalVar must NOT be set.
	diff := `diff --git a/handler.go b/handler.go
--- a/handler.go
+++ b/handler.go
@@ -10,3 +10,3 @@ func Handle() {
-	MAX_CONN := 10
+	MAX_CONN := 20
`
	hunks := ParseDiffHunks(diff)
	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(hunks))
	}
	h := hunks[0]
	if h.GlobalVar != "" {
		t.Errorf("GlobalVar should be empty when inside a function, got %q", h.GlobalVar)
	}
}

func TestParseDiffHunks_GlobalVar_LowercaseIgnored(t *testing.T) {
	// lowercase variable names must NOT trigger GlobalVar detection.
	diff := `diff --git a/svc.go b/svc.go
--- a/svc.go
+++ b/svc.go
@@ -1,2 +1,2 @@
-timeout = 10
+timeout = 20
`
	hunks := ParseDiffHunks(diff)
	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(hunks))
	}
	if hunks[0].GlobalVar != "" {
		t.Errorf("GlobalVar should be empty for lowercase var, got %q", hunks[0].GlobalVar)
	}
}

// ── 改进 6-P2: ClassName / ParentClass ──────────────────────────────────────

func TestParseDiffHunks_ClassContext_NoParent(t *testing.T) {
	// @@ context: "class Foo:"
	diff := `diff --git a/handler.py b/handler.py
--- a/handler.py
+++ b/handler.py
@@ -5,4 +5,5 @@ class Foo:
     def process(self):
+        self.log("start")
         return self._do_work()
`
	hunks := ParseDiffHunks(diff)
	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(hunks))
	}
	h := hunks[0]
	if h.ClassName != "Foo" {
		t.Errorf("ClassName = %q, want %q", h.ClassName, "Foo")
	}
	if h.ParentClass != "" {
		t.Errorf("ParentClass = %q, want empty", h.ParentClass)
	}
}

func TestParseDiffHunks_ClassContext_WithParent(t *testing.T) {
	// @@ context: "class CDfwUpdateVmNetType(CDfwOp):"
	diff := `diff --git a/ops.py b/ops.py
--- a/ops.py
+++ b/ops.py
@@ -20,4 +20,5 @@ class CDfwUpdateVmNetType(CDfwOp):
     def execute(self):
+        self.validate()
         super().execute()
`
	hunks := ParseDiffHunks(diff)
	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(hunks))
	}
	h := hunks[0]
	if h.ClassName != "CDfwUpdateVmNetType" {
		t.Errorf("ClassName = %q, want %q", h.ClassName, "CDfwUpdateVmNetType")
	}
	if h.ParentClass != "CDfwOp" {
		t.Errorf("ParentClass = %q, want %q", h.ParentClass, "CDfwOp")
	}
}

func TestParseDiffHunks_ClassContext_MethodResolved(t *testing.T) {
	// When @@ context is a class, the nearest preceding "def xxx:" in context
	// lines should appear in FuncContext as "ClassName.method_name".
	diff := `diff --git a/handler.py b/handler.py
--- a/handler.py
+++ b/handler.py
@@ -30,6 +30,7 @@ class Worker(BaseWorker):
     def run(self, task):
         data = self.fetch(task)
+        data = self.transform(data)
         return self.save(data)
`
	hunks := ParseDiffHunks(diff)
	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(hunks))
	}
	h := hunks[0]
	if h.ClassName != "Worker" {
		t.Errorf("ClassName = %q, want %q", h.ClassName, "Worker")
	}
	if h.ParentClass != "BaseWorker" {
		t.Errorf("ParentClass = %q, want %q", h.ParentClass, "BaseWorker")
	}
	// FuncContext should be resolved to "Worker.run" because "def run" appeared
	// in the context lines before the first added line.
	if !strings.Contains(h.FuncContext, "run") {
		t.Errorf("FuncContext = %q, expected it to contain 'run'", h.FuncContext)
	}
}

func TestParseDiffHunks_ClassContext_NotAClass(t *testing.T) {
	// Regular function context must NOT set ClassName.
	diff := `diff --git a/svc.go b/svc.go
--- a/svc.go
+++ b/svc.go
@@ -10,3 +10,4 @@ func (s *Svc) Handle(ctx context.Context) error {
 	doWork()
+	log.Info("done")
`
	hunks := ParseDiffHunks(diff)
	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(hunks))
	}
	h := hunks[0]
	if h.ClassName != "" {
		t.Errorf("ClassName = %q, want empty for non-class context", h.ClassName)
	}
	if h.ParentClass != "" {
		t.Errorf("ParentClass = %q, want empty", h.ParentClass)
	}
}

// ── 改进 1: ResolveFuncAtLine ─────────────────────────────────────────────────

func TestResolveFuncAtLine_Go(t *testing.T) {
	lines := []string{
		"package main",
		"",
		"func ProcessPayment(ctx context.Context, req *PayReq) error {",
		"\tif req.Amount <= 0 {",
		"\t\treturn ErrInvalidAmount",
		"\t}",
	}
	name := ResolveFuncAtLine(lines, 5, 20)
	if name != "ProcessPayment" {
		t.Errorf("ResolveFuncAtLine = %q, want %q", name, "ProcessPayment")
	}
}

func TestResolveFuncAtLine_GoMethod(t *testing.T) {
	lines := []string{
		"func (s *PaymentService) Charge(ctx context.Context) error {",
		"\tconn := s.dial()",
		"\treturn conn.Send()",
	}
	name := ResolveFuncAtLine(lines, 3, 20)
	if name != "Charge" {
		t.Errorf("ResolveFuncAtLine = %q, want %q", name, "Charge")
	}
}

func TestResolveFuncAtLine_Python(t *testing.T) {
	lines := []string{
		"class Foo:",
		"    def process(self, data):",
		"        result = transform(data)",
		"        return result",
	}
	name := ResolveFuncAtLine(lines, 4, 20)
	if name != "process" {
		t.Errorf("ResolveFuncAtLine = %q, want %q", name, "process")
	}
}

func TestResolveFuncAtLine_PythonAsync(t *testing.T) {
	lines := []string{
		"    async def fetch_data(self):",
		"        await asyncio.sleep(0)",
		"        return self.data",
	}
	name := ResolveFuncAtLine(lines, 3, 20)
	if name != "fetch_data" {
		t.Errorf("ResolveFuncAtLine = %q, want %q", name, "fetch_data")
	}
}

func TestResolveFuncAtLine_JavaScript(t *testing.T) {
	lines := []string{
		"export async function handleRequest(req, res) {",
		"  const data = await getData();",
		"  res.json(data);",
		"}",
	}
	name := ResolveFuncAtLine(lines, 3, 20)
	if name != "handleRequest" {
		t.Errorf("ResolveFuncAtLine = %q, want %q", name, "handleRequest")
	}
}

func TestResolveFuncAtLine_JavaScriptArrow(t *testing.T) {
	lines := []string{
		"const processOrder = async (order) => {",
		"  const validated = validate(order);",
		"  return save(validated);",
		"};",
	}
	name := ResolveFuncAtLine(lines, 3, 20)
	if name != "processOrder" {
		t.Errorf("ResolveFuncAtLine = %q, want %q", name, "processOrder")
	}
}

func TestResolveFuncAtLine_Ruby(t *testing.T) {
	lines := []string{
		"  def create_order(params)",
		"    order = Order.new(params)",
		"    order.save",
		"  end",
	}
	name := ResolveFuncAtLine(lines, 3, 20)
	if name != "create_order" {
		t.Errorf("ResolveFuncAtLine = %q, want %q", name, "create_order")
	}
}

func TestResolveFuncAtLine_Rust(t *testing.T) {
	lines := []string{
		"pub async fn process_batch(items: Vec<Item>) -> Result<()> {",
		"    for item in items {",
		"        handle(item).await?;",
		"    }",
		"    Ok(())",
		"}",
	}
	name := ResolveFuncAtLine(lines, 5, 20)
	if name != "process_batch" {
		t.Errorf("ResolveFuncAtLine = %q, want %q", name, "process_batch")
	}
}

func TestResolveFuncAtLine_MaxScanRespected(t *testing.T) {
	// Put the function definition beyond maxScan — should return "".
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = "\t// comment"
	}
	lines[0] = "func Far() {"
	// startLine=25, maxScan=5 → only scans back to line 20 (idx 19), won't reach line 1 (idx 0).
	name := ResolveFuncAtLine(lines, 25, 5)
	if name != "" {
		t.Errorf("ResolveFuncAtLine = %q, want empty (beyond maxScan)", name)
	}
}

func TestResolveFuncAtLine_StartBeyondLen(t *testing.T) {
	lines := []string{
		"func Small() {",
		"  return",
		"}",
	}
	// startLine > len(lines) should clamp and still find the function.
	name := ResolveFuncAtLine(lines, 100, 50)
	if name != "Small" {
		t.Errorf("ResolveFuncAtLine with oversized startLine = %q, want %q", name, "Small")
	}
}

func TestResolveFuncAtLine_EmptyLines(t *testing.T) {
	name := ResolveFuncAtLine(nil, 1, 10)
	if name != "" {
		t.Errorf("ResolveFuncAtLine on nil lines = %q, want empty", name)
	}
	name = ResolveFuncAtLine([]string{}, 1, 10)
	if name != "" {
		t.Errorf("ResolveFuncAtLine on empty lines = %q, want empty", name)
	}
}
