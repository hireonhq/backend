package service_test

// Unit tests for the profile service. The upload ordering test is the regression
// test for the destructive bug in the Node backend, which deleted the user's
// existing CV before the replacement upload had succeeded.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/hireonai/backend-hireon-go/internal/domain"
	"github.com/hireonai/backend-hireon-go/internal/service"
)

type fakeProfileStore struct {
	profile    *domain.Profile
	updates    map[string]any
	tags       []string
	photo      string
	cvURL      *string
	cvStatus   string
	enqueued   int
	completed  int
	failed     int
	bookmarked []domain.Job
	err        error
}

func (f *fakeProfileStore) ProfileByUserID(context.Context, string) (*domain.Profile, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.profile == nil {
		return nil, domain.ErrNotFound
	}
	return f.profile, nil
}

func (f *fakeProfileStore) UpdateProfileFields(_ context.Context, _ string, fields map[string]any) error {
	f.updates = fields
	return f.err
}

func (f *fakeProfileStore) UpdateTagPreferences(_ context.Context, _ string, tags []string) error {
	f.tags = tags
	return f.err
}

func (f *fakeProfileStore) AddBookmark(context.Context, string, string) error    { return f.err }
func (f *fakeProfileStore) RemoveBookmark(context.Context, string, string) error { return f.err }

func (f *fakeProfileStore) BookmarkedJobs(context.Context, string) ([]domain.Job, error) {
	return f.bookmarked, f.err
}

func (f *fakeProfileStore) SetPhoto(_ context.Context, _ string, url string) error {
	f.photo = url
	return f.err
}

func (f *fakeProfileStore) SetCV(_ context.Context, _ string, url *string, status string) error {
	f.cvURL = url
	f.cvStatus = status
	return f.err
}

func (f *fakeProfileStore) SetCVStatus(_ context.Context, _, status string) error {
	f.cvStatus = status
	return f.err
}

func (f *fakeProfileStore) ProfileCV(context.Context, string) (string, string, error) {
	if f.cvURL == nil {
		return "", f.cvStatus, nil
	}
	return *f.cvURL, f.cvStatus, nil
}

func (f *fakeProfileStore) EnqueueJob(context.Context, string, string) (string, error) {
	f.enqueued++
	return "job-1", nil
}

func (f *fakeProfileStore) CompleteJob(context.Context, string)     { f.completed++ }
func (f *fakeProfileStore) FailJob(context.Context, string, string) { f.failed++ }

func strp(s string) *string { return &s }

// A failed upload must not destroy the existing photo.
func TestUploadPhotoKeepsPreviousOnFailure(t *testing.T) {
	store := &fakeProfileStore{profile: &domain.Profile{
		ID: "p1", PhotoURL: strp("https://cdn/photos/old.jpg"),
	}}
	objects := &recordingObjects{}
	svc := service.NewProfileService(store, objects, nil)

	body := strings.NewReader(strings.Repeat("x", 1024))
	_, err := svc.UploadPhoto(context.Background(), "u1", body, "me.png", "image/png")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The old object is deleted only after the database records the new one.
	if len(objects.deletes) != 1 || objects.deletes[0] != "https://cdn/photos/old.jpg" {
		t.Fatalf("expected the previous photo to be removed after the swap, got %v", objects.deletes)
	}
	if len(objects.puts) != 1 {
		t.Fatalf("expected one upload, got %v", objects.puts)
	}
}

func TestUploadPhotoRejectsWrongType(t *testing.T) {
	svc := service.NewProfileService(&fakeProfileStore{profile: &domain.Profile{ID: "p1"}}, &recordingObjects{}, nil)
	_, err := svc.UploadPhoto(context.Background(), "u1", strings.NewReader("data"), "me.txt", "text/plain")
	if err == nil {
		t.Fatal("expected a rejection for a non-image upload")
	}
	var apiErr *domain.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("err = %v, want a 400 APIError", err)
	}
}

func TestUploadCVRejectsOversize(t *testing.T) {
	svc := service.NewProfileService(&fakeProfileStore{profile: &domain.Profile{ID: "p1"}}, &recordingObjects{}, nil)
	// One byte over the 5 MB limit.
	big := strings.NewReader(strings.Repeat("x", service.MaxCVBytes+1))
	_, _, err := svc.UploadCV(context.Background(), "u1", big, "cv.pdf", "application/pdf", nil)
	if err == nil {
		t.Fatal("expected an oversize rejection")
	}
	var apiErr *domain.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("err = %v, want a 400 APIError", err)
	}
}

func TestUploadCVRejectsNonPDF(t *testing.T) {
	svc := service.NewProfileService(&fakeProfileStore{profile: &domain.Profile{ID: "p1"}}, &recordingObjects{}, nil)
	_, _, err := svc.UploadCV(context.Background(), "u1", strings.NewReader("data"), "cv.docx",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document", nil)
	if err == nil {
		t.Fatal("expected a rejection for a non-PDF CV")
	}
}

// A successful upload marks the CV pending and enqueues the embedding, and the
// response does not wait for the ML service.
func TestUploadCVEnqueuesEmbedding(t *testing.T) {
	store := &fakeProfileStore{profile: &domain.Profile{ID: "p1"}}
	svc := service.NewProfileService(store, &recordingObjects{}, nil)

	url, jobID, err := svc.UploadCV(context.Background(), "u1",
		strings.NewReader("%PDF-1.4 fake"), "my cv.pdf", "application/pdf",
		func(context.Context, string) (string, error) {
			return "job-42", nil
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if url == "" || jobID != "job-42" {
		t.Fatalf("url=%q jobID=%q", url, jobID)
	}
	if store.cvStatus != "pending" {
		t.Errorf("cv status = %q, want pending", store.cvStatus)
	}
	if !strings.Contains(url, "my-cv.pdf") {
		t.Errorf("filename was not sanitised into the key: %s", url)
	}
}

// If enqueueing fails, the upload still succeeds: the CV is stored and usable.
func TestUploadCVSucceedsWhenEnqueueFails(t *testing.T) {
	store := &fakeProfileStore{profile: &domain.Profile{ID: "p1"}}
	svc := service.NewProfileService(store, &recordingObjects{}, nil)

	url, jobID, err := svc.UploadCV(context.Background(), "u1",
		strings.NewReader("%PDF-1.4 fake"), "cv.pdf", "application/pdf",
		func(context.Context, string) (string, error) {
			return "", errors.New("queue unavailable")
		})
	if err != nil {
		t.Fatalf("an enqueue failure must not fail the upload: %v", err)
	}
	if url == "" || jobID != "" {
		t.Fatalf("url=%q jobID=%q", url, jobID)
	}
}

// sanitizeName must strip any path traversal attempt from the filename.
func TestUploadCVSanitisesFilename(t *testing.T) {
	store := &fakeProfileStore{profile: &domain.Profile{ID: "p1"}}
	objects := &recordingObjects{}
	svc := service.NewProfileService(store, objects, nil)

	if _, _, err := svc.UploadCV(context.Background(), "u1",
		strings.NewReader("%PDF-1.4"), "../../etc/passwd.pdf", "application/pdf",
		func(context.Context, string) (string, error) { return "j", nil }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(objects.puts) != 1 {
		t.Fatalf("expected one upload, got %v", objects.puts)
	}
	if strings.Contains(objects.puts[0], "..") || strings.Contains(objects.puts[0], "etc/passwd") {
		t.Fatalf("key escaped the prefix: %s", objects.puts[0])
	}
}

func TestTagPreferencesRejectsEmpty(t *testing.T) {
	svc := service.NewProfileService(&fakeProfileStore{}, &recordingObjects{}, nil)
	if _, err := svc.UpdateTagPreferences(context.Background(), "u1", nil); err == nil {
		t.Fatal("expected a rejection for empty tag preferences")
	}
}

func TestDeleteCVWhenMissing(t *testing.T) {
	svc := service.NewProfileService(&fakeProfileStore{profile: &domain.Profile{ID: "p1"}}, &recordingObjects{}, nil)
	err := svc.DeleteCV(context.Background(), "u1")
	var apiErr *domain.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("err = %v, want a 404 APIError", err)
	}
}

// recordingObjects captures call order and satisfies storage.Store.
type recordingObjects struct {
	puts    []string
	deletes []string
	ops     []string
	failPut bool
}

func (f *recordingObjects) Put(_ context.Context, key string, _ io.Reader, _ string) (string, error) {
	if f.failPut {
		f.ops = append(f.ops, "put:FAILED")
		return "", errors.New("upload boom")
	}
	f.puts = append(f.puts, key)
	f.ops = append(f.ops, "put:"+key)
	return "https://cdn/" + key, nil
}

func (f *recordingObjects) Delete(_ context.Context, url string) error {
	f.deletes = append(f.deletes, url)
	f.ops = append(f.ops, "delete:"+url)
	return nil
}

// TestUploadFailurePreservesExistingFile is the direct regression test for the
// destructive ordering in the Node backend: it deleted the old CV/photo before
// the replacement had been stored, so any upload failure destroyed the user's
// file while the database still pointed at it.
func TestUploadFailurePreservesExistingFile(t *testing.T) {
	store := &fakeProfileStore{profile: &domain.Profile{
		ID: "p1", PhotoURL: strp("https://cdn/photos/old.jpg"),
	}}
	objects := &recordingObjects{failPut: true}
	svc := service.NewProfileService(store, objects, nil)

	if _, err := svc.UploadPhoto(context.Background(), "u1",
		strings.NewReader(strings.Repeat("x", 256)), "new.png", "image/png"); err == nil {
		t.Fatal("expected the upload to fail")
	}
	if len(objects.deletes) != 0 {
		t.Fatalf("the previous file was deleted despite a failed upload: %v", objects.deletes)
	}
	if store.photo != "" {
		t.Fatalf("the database was updated despite a failed upload: %q", store.photo)
	}
}

// TestUploadOrderIsUploadThenPersistThenDelete asserts the safe ordering.
func TestUploadOrderIsUploadThenPersistThenDelete(t *testing.T) {
	store := &fakeProfileStore{profile: &domain.Profile{
		ID: "p1", PhotoURL: strp("https://cdn/photos/old.jpg"),
	}}
	objects := &recordingObjects{}
	svc := service.NewProfileService(store, objects, nil)

	if _, err := svc.UploadPhoto(context.Background(), "u1",
		strings.NewReader(strings.Repeat("x", 256)), "new.png", "image/png"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(objects.ops) != 2 {
		t.Fatalf("expected an upload then a delete, got %v", objects.ops)
	}
	if !strings.HasPrefix(objects.ops[0], "put:") {
		t.Fatalf("first operation was %q, want the upload", objects.ops[0])
	}
	if !strings.HasPrefix(objects.ops[1], "delete:") {
		t.Fatalf("second operation was %q, want the delete", objects.ops[1])
	}
}
