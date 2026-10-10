// Package domain holds the core types and sentinel errors. It imports nothing
// from the HTTP or database layers, so the service layer stays testable without
// a server -- unlike the Node services, one of which dereferenced the Hapi `h`
// response toolkit from inside a service function.
package domain

import "errors"

// Sentinel errors. Handlers map these to status codes in one place, so a service
// never needs to know what an HTTP status is.
var (
	ErrNotFound     = errors.New("not found")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrConflict     = errors.New("conflict")
	ErrValidation   = errors.New("validation failed")
	ErrUpstream     = errors.New("upstream service error")
	ErrUpstreamSlow = errors.New("upstream service timeout")
)

// APIError carries a message that is safe to return to the client.
type APIError struct {
	Status  int
	Message string
	Err     error
}

func (e *APIError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *APIError) Unwrap() error { return e.Err }

// NewAPIError builds a client-safe error with an optional wrapped cause. The
// cause is logged but never sent to the client, so internal details (file paths,
// driver strings, upstream stack traces) cannot leak. The old backend returned
// `str(e)` straight to callers.
func NewAPIError(status int, message string, cause error) *APIError {
	return &APIError{Status: status, Message: message, Err: cause}
}

type Pagination struct {
	CurrentPage  int  `json:"currentPage"`
	TotalPages   int  `json:"totalPages"`
	TotalItems   int  `json:"totalItems"`
	ItemsPerPage int  `json:"itemsPerPage"`
	HasNextPage  bool `json:"hasNextPage"`
	HasPrevPage  bool `json:"hasPrevPage"`
}

func NewPagination(page, limit, totalItems int) Pagination {
	totalPages := 0
	if limit > 0 {
		totalPages = (totalItems + limit - 1) / limit
	}
	return Pagination{
		CurrentPage:  page,
		TotalPages:   totalPages,
		TotalItems:   totalItems,
		ItemsPerPage: limit,
		HasNextPage:  page < totalPages,
		HasPrevPage:  page > 1,
	}
}

// Reference types -------------------------------------------------------------

type CompanyIndustry struct {
	ID        string    `json:"_id"`
	Name      string    `json:"name"`
	Version   int       `json:"__v"`
	CreatedAt MongoTime `json:"createdAt"`
	UpdatedAt MongoTime `json:"updatedAt"`
}

type JobCategory struct {
	ID        string    `json:"_id"`
	Name      string    `json:"name"`
	Version   int       `json:"__v"`
	CreatedAt MongoTime `json:"createdAt"`
	UpdatedAt MongoTime `json:"updatedAt"`
}

type JobMinExperience struct {
	ID        string    `json:"_id"`
	Name      string    `json:"name"`
	Version   int       `json:"__v"`
	CreatedAt MongoTime `json:"createdAt"`
	UpdatedAt MongoTime `json:"updatedAt"`
}

// Job shapes ------------------------------------------------------------------

type IndustryRef struct {
	ID   string `json:"_id"`
	Name string `json:"name"`
}

// Ref is the shape Mongo populate() produced whenever a projection was given:
// only _id and name survive. Relation fields therefore omit the document
// bookkeeping (__v, createdAt, updatedAt) that a full document would carry.
type Ref struct {
	ID   string `json:"_id"`
	Name string `json:"name"`
}

type CompanyRef struct {
	ID       string       `json:"_id"`
	Name     string       `json:"name"`
	Industry *IndustryRef `json:"industry,omitempty"`
}

type Job struct {
	ID                    string      `json:"_id"`
	Categories            []Ref       `json:"categories"`
	URL                   string      `json:"url"`
	JobPosition           string      `json:"jobPosition"`
	EmploymentType        string      `json:"employmentType"`
	WorkingLocationType   string      `json:"workingLocationType"`
	WorkingLocation       *string     `json:"workingLocation"`
	MinSalary             int64       `json:"minSalary"`
	MaxSalary             int64       `json:"maxSalary"`
	JobDescList           []string    `json:"jobDescList"`
	JobQualificationsList []string    `json:"jobQualificationsList"`
	Company               *CompanyRef `json:"company,omitempty"`
	MinExperience         *Ref        `json:"minExperience,omitempty"`
	ScoreMatch            *string     `json:"scoreMatch"`
	Version               int         `json:"__v"`
	CreatedAt             MongoTime   `json:"createdAt"`
	UpdatedAt             MongoTime   `json:"updatedAt"`
}

type JobDetail struct {
	Job
	AnalysisResult *AnalysisResult `json:"analysisResult"`
}

type AnalysisResult struct {
	ID               string  `json:"_id"`
	UserID           string  `json:"userId"`
	JobID            string  `json:"jobId"`
	CVRelevanceScore float64 `json:"cvRelevanceScore"`
	Explanation      string  `json:"explanation"`
	// Field name keeps the typo from the Mongo schema: the frontend already
	// depends on it, so renaming would be a breaking API change.
	SkilIdentificationDict map[string]any `json:"skilIdentificationDict"`
	Suggestions            []string       `json:"suggestions"`
	Improvements           []string       `json:"improvements"`
	CreatedAt              any            `json:"createdAt"`
	UpdatedAt              any            `json:"updatedAt"`
}

// Profile shapes --------------------------------------------------------------

type UserRef struct {
	ID         string     `json:"_id"`
	Username   string     `json:"username"`
	Email      string     `json:"email"`
	VerifiedAt *MongoTime `json:"verifiedAt"`
}

type Profile struct {
	ID             string    `json:"_id"`
	Fullname       string    `json:"fullname"`
	Phone          *string   `json:"phone"`
	Domicile       *string   `json:"domicile"`
	LastEducation  *string   `json:"lastEducation"`
	PhotoURL       *string   `json:"photoUrl"`
	PortfolioURL   *string   `json:"portfolioUrl"`
	CVURL          *string   `json:"cvUrl"`
	CVStatus       string    `json:"cvStatus"`
	TagPreferences []string  `json:"tagPreferences"`
	BookmarkJobs   []Job     `json:"bookmarkJobs"`
	User           *UserRef  `json:"user"`
	Version        int       `json:"__v"`
	CreatedAt      MongoTime `json:"createdAt"`
	UpdatedAt      MongoTime `json:"updatedAt"`
}

type User struct {
	ID            string
	Username      string
	Email         string
	SupabaseID    string
	Role          string
	PhotoURL      string
	OAuthProvider string
	VerifiedAt    any
}

// QueuedJob is a claimed unit of background work.
type QueuedJob struct {
	ID       string
	UserID   string
	Kind     string
	Attempts int
}

// QueuedJobStatus is the externally visible state of a background job.
type QueuedJobStatus struct {
	ID        string         `json:"jobId"`
	Kind      string         `json:"kind"`
	Status    string         `json:"status"`
	Attempts  int            `json:"attempts"`
	Error     *string        `json:"error"`
	Result    map[string]any `json:"result,omitempty"`
	CreatedAt MongoTime      `json:"createdAt"`
	UpdatedAt MongoTime      `json:"updatedAt"`
}
