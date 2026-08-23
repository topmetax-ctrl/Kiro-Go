package apikey

import (
	"database/sql"
	"strings"
	"time"
)

// ProviderErrorDetail is the admin-only diagnostic for one upstream attempt.
// Portal queries never join this table.
type ProviderErrorDetail struct {
	RequestID         string    `json:"requestId"`
	Attempt           int       `json:"attempt"`
	ProviderID        string    `json:"providerId,omitempty"`
	ProviderName      string    `json:"providerName,omitempty"`
	ConnectionID      string    `json:"connectionId,omitempty"`
	ConnectionName    string    `json:"connectionName,omitempty"`
	AccountID         string    `json:"accountId,omitempty"`
	Endpoint          string    `json:"endpoint,omitempty"`
	ClientModel       string    `json:"clientModel,omitempty"`
	EffectiveModel    string    `json:"effectiveModel,omitempty"`
	UpstreamStatus    int       `json:"upstreamStatus"`
	UpstreamCode      string    `json:"upstreamCode,omitempty"`
	UpstreamMessage   string    `json:"upstreamMessage,omitempty"`
	UpstreamRequestID string    `json:"upstreamRequestId,omitempty"`
	RetryAfter        string    `json:"retryAfter,omitempty"`
	Category          string    `json:"category,omitempty"`
	PublicCode        string    `json:"publicCode,omitempty"`
	Detail            string    `json:"detail,omitempty"`
	DetailTruncated   bool      `json:"detailTruncated"`
	CreatedAt         time.Time `json:"createdAt"`
}

// PutProviderErrorDetail upserts a redacted diagnostic. Empty requestID is ignored.
func (s *Service) PutProviderErrorDetail(d ProviderErrorDetail) error {
	if s == nil || s.db == nil || strings.TrimSpace(d.RequestID) == "" {
		return nil
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = s.now().UTC()
	}
	_, err := s.db.Exec(`INSERT INTO provider_error_details(
		request_id,attempt,connection_id,connection_name,provider_id,provider_name,account_id,endpoint,client_model,effective_model,
		upstream_status,upstream_code,upstream_message,upstream_request_id,retry_after,category,public_code,
		detail,detail_truncated,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(request_id,attempt,connection_id) DO UPDATE SET
			connection_name=excluded.connection_name,
			provider_id=excluded.provider_id,
			provider_name=excluded.provider_name,
			account_id=excluded.account_id,
			endpoint=excluded.endpoint,
			client_model=excluded.client_model,
			effective_model=excluded.effective_model,
			upstream_status=excluded.upstream_status,
			upstream_code=excluded.upstream_code,
			upstream_message=excluded.upstream_message,
			upstream_request_id=excluded.upstream_request_id,
			retry_after=excluded.retry_after,
			category=excluded.category,
			public_code=excluded.public_code,
			detail=excluded.detail,
			detail_truncated=excluded.detail_truncated,
			created_at=excluded.created_at`,
		d.RequestID, d.Attempt, d.ConnectionID, d.ConnectionName, d.ProviderID, d.ProviderName, d.AccountID, d.Endpoint, d.ClientModel, d.EffectiveModel,
		d.UpstreamStatus, d.UpstreamCode, d.UpstreamMessage, d.UpstreamRequestID, d.RetryAfter, d.Category, d.PublicCode,
		d.Detail, boolInt(d.DetailTruncated), d.CreatedAt.Unix())
	if err != nil {
		ObserveSQL(err, true)
		return err
	}
	return nil
}

// GetProviderErrorDetails returns every stored attempt for a request, oldest first.
func (s *Service) GetProviderErrorDetails(requestID string) ([]ProviderErrorDetail, error) {
	if s == nil || s.db == nil {
		return nil, ErrUnavailable
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, ErrNotFound
	}
	// Ordered by attempt, then by the order the keys were tried within it — created_at
	// alone cannot separate two keys rejected in the same second.
	rows, err := s.db.Query(`SELECT request_id,attempt,connection_id,connection_name,provider_id,provider_name,account_id,endpoint,client_model,effective_model,
		upstream_status,upstream_code,upstream_message,upstream_request_id,retry_after,category,public_code,
		detail,detail_truncated,created_at
		FROM provider_error_details WHERE request_id=? ORDER BY attempt ASC, created_at ASC, connection_id ASC`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderErrorDetail
	for rows.Next() {
		var d ProviderErrorDetail
		var trunc, created int64
		if err := rows.Scan(&d.RequestID, &d.Attempt, &d.ConnectionID, &d.ConnectionName, &d.ProviderID, &d.ProviderName, &d.AccountID, &d.Endpoint, &d.ClientModel, &d.EffectiveModel,
			&d.UpstreamStatus, &d.UpstreamCode, &d.UpstreamMessage, &d.UpstreamRequestID, &d.RetryAfter, &d.Category, &d.PublicCode,
			&d.Detail, &trunc, &created); err != nil {
			return nil, err
		}
		d.DetailTruncated = trunc == 1
		d.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

func (s *Service) cleanupProviderErrors(cut int64) (int, error) {
	n := 0
	for {
		res, err := s.db.Exec(`DELETE FROM provider_error_details WHERE request_id IN (
			SELECT request_id FROM provider_error_details WHERE created_at < ? LIMIT 500)`, cut)
		if err != nil {
			if err == sql.ErrNoRows {
				return n, nil
			}
			return n, err
		}
		got, _ := res.RowsAffected()
		n += int(got)
		if got < 500 {
			return n, nil
		}
	}
}
