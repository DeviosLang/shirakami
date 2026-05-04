package tool

import (
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
