package handlers

import "github.com/personals3/api/internal/middleware"

// quotaAdjust is the post-write quota reconciliation hook: after bytes hit
// disk, handlers charge/refund the difference between the pre-reservation
// and what actually arrived. It is exactly middleware.QuotaReserve; a
// package var only so regression tests can inject a failure at the
// reconciliation point without touching the surrounding reserve/refund
// calls (which must stay real for the cleanup paths under test).
var quotaAdjust = middleware.QuotaReserve
