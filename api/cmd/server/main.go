// PersonalS3 API server — Parts 1-4 wired together.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/google/uuid"

	"github.com/personals3/api/internal/cache"
	"github.com/personals3/api/internal/config"
	"github.com/personals3/api/internal/db"
	"github.com/personals3/api/internal/email"
	"github.com/personals3/api/internal/handlers"
	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/importer"
	"github.com/personals3/api/internal/livefeed"
	mw "github.com/personals3/api/internal/middleware"
	"github.com/personals3/api/internal/storage"
)

// getenv returns the env value or a fallback when missing/empty.
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.JWTSecret == "" {
		log.Fatalf("JWT_SECRET is required (generate with: openssl rand -hex 32)")
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer pool.Close()
	log.Printf("connected to postgres")

	// Drain QuotaEvent broadcasts into the persistent quota_events table.
	// Must be running before any QuotaReserve call fires.
	mw.StartQuotaEventPersister(pool)

	rdb, err := cache.Connect(ctx, cfg.ValkeyURL)
	if err != nil {
		log.Fatalf("valkey connect: %v", err)
	}
	defer rdb.Close()
	log.Printf("connected to valkey")

	fs, err := storage.New(cfg.StorageRoot)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	log.Printf("storage root: %s", cfg.StorageRoot)

	// Email mailer — SMTP when configured, file-outbox in dev.
	mailer := email.New(email.Load(cfg.StorageRoot))
	adminEmail := getenv("ADMIN_EMAIL", "")
	publicURL := getenv("PUBLIC_BASE_URL", "http://localhost:8080")
	// supportEmail is what users see in transactional email bodies as
	// the "Need help?" address. Prefer SMTP_REPLY_TO (Cloudflare-routed
	// real inbox), fall back to admin email so things still render.
	supportEmail := getenv("SMTP_REPLY_TO", adminEmail)

	totpH := &handlers.TOTPHandler{DB: pool, JWTSecret: cfg.JWTSecret, Issuer: "PersonalS3"}
	authH := &handlers.AuthHandler{
		DB: pool, FS: fs, JWTSecret: cfg.JWTSecret, TOTP: totpH,
		Mailer: mailer, AdminEmail: adminEmail, SupportEmail: supportEmail,
		LoginURL: publicURL + "/login",
		AdminURL: publicURL,
		DefaultQuotaBytes: cfg.DefaultQuotaBytes,
	}
	requestsH := &handlers.AdminRequestsHandler{
		DB: pool, Mailer: mailer, OutboxDir: email.Load(cfg.StorageRoot).OutboxDir,
		StorageRoot: cfg.StorageRoot,
		AdminEmail: adminEmail, SupportEmail: supportEmail,
		LoginURL: publicURL + "/login",
		DefaultQuotaBytes: cfg.DefaultQuotaBytes,
	}
	bucketH := &handlers.BucketHandler{DB: pool, FS: fs}
	objectH := &handlers.ObjectHandler{DB: pool, FS: fs, RDB: rdb}
	mpH := &handlers.MultipartHandler{DB: pool, FS: fs}
	adminH := &handlers.AdminHandler{DB: pool, RDB: rdb, FS: fs, StorageRoot: cfg.StorageRoot}
	cleanupH := &handlers.CleanupHandler{DB: pool, StorageRoot: cfg.StorageRoot}
	shardH := &handlers.ShardHandler{DB: pool}
	searchH := &handlers.SearchHandler{DB: pool}
	sharesH := &handlers.SharesHandler{DB: pool, JWTSecret: cfg.JWTSecret}
	s3CredsH := &handlers.S3CredsHandler{DB: pool}
	importH := &handlers.ImportHandler{DB: pool}
	shareH := &handlers.ShareHandler{DB: pool, FS: fs, JWTSecret: cfg.JWTSecret, MP: mpH}
	publicH := &handlers.PublicHandler{DB: pool, FS: fs}
	trashH := &handlers.TrashHandler{DB: pool, FS: fs, RDB: rdb}

	// Background importer — pulls URL-import jobs out of the DB and runs them
	// as background goroutines so the HTTP request that enqueued them can
	// return instantly. The browser polls /imports for progress.
	im := &importer.Importer{
		DB:          pool,
		FS:          fs,
		Concurrency: 2,
		WorkerName:  "api",
		EnqueueTranscode: func(ctx context.Context, objectID, bucketID uuid.UUID, key, ct string) {
			handlers.EnqueueTranscodeFromImporter(ctx, pool, fs, objectID, bucketID, key, ct)
		},
		QuotaReserve: func(ctx context.Context, userID uuid.UUID, delta int64) error {
			return mw.QuotaReserve(ctx, pool, userID, delta)
		},
		CheckDiskHealthy: func(ctx context.Context) error {
			return mw.CheckDiskHealthy(ctx, pool, cfg.StorageRoot)
		},
	}
	im.Start(ctx)

	// dispatch by query string — S3 uses ?uploads, ?uploadId, ?partNumber to
	// distinguish multipart from single-object operations on the same path.
	putDispatch := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("uploadId") != "" {
			mpH.UploadPart(w, r)
			return
		}
		objectH.PutObject(w, r)
	}
	postDispatch := func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if _, hasUploads := q["uploads"]; hasUploads {
			mpH.Initiate(w, r)
			return
		}
		if q.Get("uploadId") != "" {
			mpH.Complete(w, r)
			return
		}
		if _, hasImport := q["import"]; hasImport {
			objectH.ImportFromURL(w, r)
			return
		}
		if _, hasTranscode := q["transcode"]; hasTranscode {
			objectH.ManuallyTranscode(w, r)
			return
		}
		if _, hasPresign := q["presign"]; hasPresign {
			shareH.CreatePresignedURL(w, r)
			return
		}
		if _, hasPresignMP := q["presign-multipart"]; hasPresignMP {
			shareH.CreatePresignedMultipart(w, r)
			return
		}
		if _, hasRestore := q["restore"]; hasRestore {
			objectH.RestoreVersion(w, r)
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "BAD_REQUEST",
			"POST requires ?uploads, ?uploadId=..., ?import, ?transcode, ?presign, ?presign-multipart, or ?restore")
	}
	deleteDispatch := func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("uploadId") != "" {
			mpH.Abort(w, r)
			return
		}
		if _, hasTranscode := q["transcode"]; hasTranscode {
			objectH.DeleteTranscodes(w, r)
			return
		}
		if q.Get("versionId") != "" {
			objectH.DeleteVersion(w, r)
			return
		}
		// ?purge = skip the trash and hard-delete immediately. Refunds quota.
		if _, hasPurge := q["purge"]; hasPurge {
			objectH.PurgeObject(w, r)
			return
		}
		objectH.DeleteObject(w, r)
	}
	getDispatch := func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("uploadId") != "" {
			mpH.ListParts(w, r)
			return
		}
		if _, hasInfo := q["info"]; hasInfo {
			objectH.ObjectInfo(w, r)
			return
		}
		if _, hasVersions := q["versions"]; hasVersions {
			objectH.ListVersions(w, r)
			return
		}
		if q.Get("versionId") != "" {
			objectH.GetVersion(w, r)
			return
		}
		objectH.GetObject(w, r)
	}

	// Live telemetry broker — privacy-scrubbed public event stream for
	// live.personals3.tech (see internal/livefeed). The middleware samples
	// request/upload/download/error events; the goroutines add 5s stats
	// and transcode lifecycle events read from the jobs table.
	feed := livefeed.NewBroker()
	feed.StartStats(ctx, pool, cfg.StorageRoot)
	feed.StartTranscodePoller(ctx, pool)

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(feed.HTTPEvents())
	// No chi.Timeout — multipart part uploads on slow links can legitimately
	// take many minutes. Per-request context cancellation still works when
	// the client disconnects (Go's http server handles that). Nginx caps at
	// 600s per request, which is the real ceiling.

	// Public routes
	r.Get("/live", feed.ServeSSE) // SSE — no auth; nothing sensitive by construction
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("db down"))
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	r.Post("/auth/login",            authH.Login)
	r.Post("/auth/2fa/login",        totpH.Login)
	// Self-serve onboarding — all public; rate-limited at the router level.
	r.Post("/auth/request-account",  authH.RequestAccount)
	r.Post("/auth/forgot-password",  authH.Forgot)
	r.Post("/auth/reset-password",   authH.Reset)
	r.Post("/auth/verify-email",     authH.VerifyEmail)

	// Public share routes — signed URL is the credential, no Authorization header.
	r.Get("/share/{bucket}/*",  shareH.ServePresigned)
	r.Head("/share/{bucket}/*", shareH.ServePresigned)
	r.Put("/share/{bucket}/*",  shareH.ServePresignedPUT)   // pre-signed direct uploads
	// Pre-signed multipart finalize / abort — POST with ?finalize=1 or ?abort=1
	r.Post("/share/{bucket}/*", shareH.ServePresignedMultipartFinalize)

	// Opaque short form: /s/<token>. The token IS the credential; the URL
	// leaks no path, expiry, or method. Same revocation/expiry semantics
	// as the legacy /share path — just a tidier URL surface.
	r.Get("/s/{token}",  shareH.ServeOpaqueShare)
	r.Head("/s/{token}", shareH.ServeOpaqueShare)
	r.Put("/s/{token}",  shareH.ServeOpaqueShare)

	// Public bucket routes — bucket.is_public flag is the sole gate.
	// /public/{bucket}/ → index.html or JSON listing; /public/{bucket}/{key} → bytes.
	r.Get("/public/{bucket}",    publicH.Serve)  // no trailing slash → treat as dir
	r.Head("/public/{bucket}",   publicH.Serve)
	r.Get("/public/{bucket}/*",  publicH.Serve)
	r.Head("/public/{bucket}/*", publicH.Serve)

	// Authenticated subtree
	r.Group(func(r chi.Router) {
		r.Use(mw.Authenticator(pool, cfg.JWTSecret))
		r.Use(mw.RateLimit(rdb, 1000))
		r.Use(mw.Audit(pool))

		// Auth (current user, key management)
		r.Get("/auth/me", authH.Me)
		r.Get("/auth/storage", authH.Storage)
		// Self-service quota request: user asks for more space, admin decides
		r.Post("/auth/me/quota-request", authH.RequestQuota)
		r.Get("/auth/me/quota-request",  authH.MyQuotaRequest)
		// Self-delete — two-step (request OTP, then DELETE with code)
		r.Post("/auth/me/delete-request", authH.RequestDelete)
		r.Delete("/auth/me",              authH.DeleteSelf)

		// Trusted devices — list / revoke one. Tokens are minted by
		// /auth/2fa/login when ?trust=<label> is present in the query.
		r.Get("/auth/me/devices",         authH.ListDevices)
		r.Delete("/auth/me/devices/{id}", authH.RevokeDevice)

		// Docs are public — list + HTML are registered above the auth
		// block. Anything sensitive (operator handbook etc.) would live
		// at a different prefix.

		// TOTP 2FA — setup / verify / disable / regenerate recovery codes.
		r.Post("/auth/2fa/setup",           totpH.Setup)
		r.Post("/auth/2fa/verify",          totpH.Verify)
		r.Post("/auth/2fa/disable",         totpH.Disable)
		r.Post("/auth/2fa/recovery/regen",  totpH.RegenerateRecovery)
		r.Get("/auth/activity", authH.Activity)
		r.Get("/auth/active", authH.Active)                       // what's running NOW
		r.Delete("/auth/active/uploads/{uploadId}", authH.AbortUpload)
		r.Get("/auth/keys", authH.ListKeys)
		r.Post("/auth/keys", authH.CreateKey)
		r.Delete("/auth/keys/{id}", authH.DeleteKey)

		// S3-style credentials (AKID + secret) for AWS SDK / aws-cli compatibility
		r.Get("/auth/s3-credentials",           s3CredsH.List)
		r.Post("/auth/s3-credentials",          s3CredsH.Create)
		r.Delete("/auth/s3-credentials/{akid}", s3CredsH.Delete)

		// Admin (role=admin only). All under /admin/ — won't collide with buckets
		// because the bucket name regex forbids leading-dot/hyphen and short paths.
		// (We rely on /admin not being a valid bucket name in practice.)
		r.Route("/admin", func(r chi.Router) {
			r.Use(mw.AdminOnly)
			r.Get("/users",         adminH.ListUsers)
			r.Post("/users",        adminH.CreateUser)
			r.Patch("/users/{id}",  adminH.UpdateUser)
			r.Delete("/users/{id}", adminH.DeactivateUser)
			r.Get("/audit",         adminH.ListAudit)
			r.Get("/stats",         adminH.Stats)
			r.Get("/quota-events",         adminH.QuotaEvents)        // SSE — live quota stream
			r.Get("/quota-events/history", adminH.QuotaEventsHistory) // paginated history
			r.Get("/transcode-jobs",       adminH.TranscodeJobs)

			// Operator control over running transcode work
			r.Delete("/transcode-jobs/{id}",       adminH.CancelTranscodeJob)
			r.Post("/transcode-jobs/{id}/retry",   adminH.RetryTranscodeJob)
			r.Patch("/transcode-jobs/{id}",        adminH.UpdateTranscodeJob)
			r.Post("/objects/{id}/retry-transcode", adminH.RetryObjectTranscode)

			// Onboarding queue
			r.Get("/requests",                            requestsH.List)
			r.Post("/requests/account/{id}/approve",      requestsH.ApproveAccount)
			r.Post("/requests/account/{id}/deny",         requestsH.DenyAccount)
			r.Post("/requests/quota/{id}/approve",        requestsH.ApproveQuota)
			r.Post("/requests/quota/{id}/deny",           requestsH.DenyQuota)

			// Dev-mode email outbox
			r.Get("/email-outbox",     requestsH.OutboxList)
			r.Get("/email-outbox/*",   requestsH.OutboxGet)
			r.Get("/system-config",       adminH.GetSystemConfig)
			r.Patch("/system-config/{key}", adminH.UpdateSystemConfig)

			// Cleaner visibility + manual trigger
			r.Get("/cleanup",                cleanupH.ListRuns)
			r.Get("/cleanup/runs/{id}/log", cleanupH.RunLog)
			r.Post("/cleanup/run",           cleanupH.RunNow)

			// V4 shard tree per bucket
			r.Get("/buckets/{name}/shards", shardH.Tree)
		})

		// Async import jobs (listed under /imports — not a valid bucket name)
		r.Get("/imports",         importH.ListImports)
		r.Delete("/imports/{id}", importH.CancelOrDelete)

		// Cross-bucket search
		r.Get("/search", searchH.Search)

		// Share-link management (presigned URL lifecycle)
		r.Get("/shares",          sharesH.List)
		r.Patch("/shares/{id}",   sharesH.Patch)
		r.Delete("/shares/{id}",  sharesH.Revoke)
		r.Delete("/shares",       sharesH.RevokeAll)

		// Trash bin — soft-deleted objects across all of the user's buckets.
		// "trash" is forbidden as a bucket name (reserved), so this can't collide.
		r.Get("/trash",          trashH.List)
		r.Post("/trash",         trashH.Restore)  // ?restore is implicit; POST = restore
		r.Delete("/trash",       trashH.Purge)    // body + optional ?all=1

		// Bucket operations
		r.Get("/", bucketH.ListBuckets)
		r.Put("/{bucket}", bucketH.CreateBucket)
		r.Patch("/{bucket}", bucketH.UpdateBucket)
		r.Head("/{bucket}", bucketH.HeadBucket)
		r.Delete("/{bucket}", bucketH.DeleteBucket)   // ?force=true to wipe contents
		r.Get("/{bucket}", func(w http.ResponseWriter, r *http.Request) {
			// GET /:bucket?uploads → ListMultipartUploads (S3-style + JSON)
			if _, ok := r.URL.Query()["uploads"]; ok {
				mpH.ListUploads(w, r)
				return
			}
			objectH.ListObjects(w, r)
		})
		r.Post("/{bucket}", func(w http.ResponseWriter, r *http.Request) {
			// POST /:bucket?delete — bulk-delete N keys at once
			if _, ok := r.URL.Query()["delete"]; ok {
				objectH.BulkDelete(w, r)
				return
			}
			httpx.WriteError(w, http.StatusBadRequest, "BAD_REQUEST",
				"POST on bucket requires ?delete (with {keys:[...]} body)")
		})

		// Object + multipart on the same route — dispatch by query string.
		r.Put("/{bucket}/*", putDispatch)
		r.Post("/{bucket}/*", postDispatch)
		r.Get("/{bucket}/*", getDispatch)
		r.Head("/{bucket}/*", objectH.HeadObject)
		r.Delete("/{bucket}/*", deleteDispatch)
	})

	addr := fmt.Sprintf("%s:%d", cfg.APIHost, cfg.APIPort)
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		log.Printf("listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, sCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer sCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
