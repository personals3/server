package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/auth"
	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
)

type S3CredsHandler struct {
	DB *pgxpool.Pool
}

type s3CredCreateResp struct {
	AccessKeyID     string    `json:"accessKeyId"`
	SecretAccessKey string    `json:"secretAccessKey"`  // shown ONCE
	Name            string    `json:"name"`
	CreatedAt       time.Time `json:"createdAt"`
	Hint            string    `json:"hint"`
}

type s3CredDTO struct {
	AccessKeyID string     `json:"accessKeyId"`
	Name        string     `json:"name"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
}

// POST /auth/s3-credentials
func (h *S3CredsHandler) Create(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())

	var req struct{ Name string `json:"name"` }
	_ = json.NewDecoder(r.Body).Decode(&req)

	akid, secret, err := auth.GenerateS3Credentials()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "GEN", err.Error())
		return
	}

	var createdAt time.Time
	err = h.DB.QueryRow(r.Context(), `
		INSERT INTO s3_credentials (access_key_id, user_id, secret_key, name)
		VALUES ($1, $2, $3, NULLIF($4, ''))
		RETURNING created_at`,
		akid, u.ID, secret, req.Name,
	).Scan(&createdAt)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, s3CredCreateResp{
		AccessKeyID:     akid,
		SecretAccessKey: secret,
		Name:            req.Name,
		CreatedAt:       createdAt,
		Hint: "Configure aws-cli with:\n" +
			"  aws configure set aws_access_key_id " + akid + "\n" +
			"  aws configure set aws_secret_access_key <secret>\n" +
			"  aws configure set region us-east-1\n" +
			"Then use --endpoint-url=https://<your-host>/api",
	})
}

// GET /auth/s3-credentials
func (h *S3CredsHandler) List(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	rows, err := h.DB.Query(r.Context(), `
		SELECT access_key_id, COALESCE(name, ''), last_used_at, created_at
		  FROM s3_credentials WHERE user_id = $1 ORDER BY created_at DESC`, u.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	out := []s3CredDTO{}
	for rows.Next() {
		var c s3CredDTO
		if err := rows.Scan(&c.AccessKeyID, &c.Name, &c.LastUsedAt, &c.CreatedAt); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "SCAN", err.Error())
			return
		}
		out = append(out, c)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

// DELETE /auth/s3-credentials/:akid
func (h *S3CredsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	akid := chi.URLParam(r, "akid")
	tag, err := h.DB.Exec(r.Context(),
		`DELETE FROM s3_credentials WHERE access_key_id = $1 AND user_id = $2`,
		akid, u.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.WriteError(w, http.StatusNotFound, "NOT_FOUND", "credential not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

