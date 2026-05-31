package handlers

// S3-compatible XML responses for AWS SDK / aws-cli clients.
//
// Detection: a request "looks like an S3 client" if it sent an
// AWS4-HMAC-SHA256 Authorization header OR has the "x-amz-content-sha256" or
// "x-amz-date" header. In those cases we emit XML; otherwise we use JSON.
//
// This file holds the XML types and a tiny helper. The handlers themselves
// (in buckets.go, objects.go) decide which to use.

import (
	"encoding/xml"
	"net/http"
	"strings"
	"time"
)

const s3XMLNS = "http://s3.amazonaws.com/doc/2006-03-01/"

// isS3Client returns true if the request looks like an AWS SDK call.
func isS3Client(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		return true
	}
	if r.Header.Get("X-Amz-Content-Sha256") != "" {
		return true
	}
	if r.Header.Get("X-Amz-Date") != "" {
		return true
	}
	return false
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}

// writeS3Error writes an S3-style XML error.
func writeS3Error(w http.ResponseWriter, status int, code, message, resource string) {
	type errBody struct {
		XMLName   xml.Name `xml:"Error"`
		Code      string   `xml:"Code"`
		Message   string   `xml:"Message"`
		Resource  string   `xml:"Resource,omitempty"`
		RequestID string   `xml:"RequestId,omitempty"`
	}
	writeXML(w, status, errBody{Code: code, Message: message, Resource: resource})
}

// ----- ListBuckets ---------------------------------------------------------

type s3Owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type s3Bucket struct {
	Name         string    `xml:"Name"`
	CreationDate time.Time `xml:"CreationDate"`
}

type listAllMyBucketsResult struct {
	XMLName xml.Name   `xml:"ListAllMyBucketsResult"`
	XMLNS   string     `xml:"xmlns,attr"`
	Owner   s3Owner    `xml:"Owner"`
	Buckets struct {
		Bucket []s3Bucket `xml:"Bucket"`
	} `xml:"Buckets"`
}

// ----- ListObjectsV2 -------------------------------------------------------

type s3Object struct {
	Key          string    `xml:"Key"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
	Size         int64     `xml:"Size"`
	StorageClass string    `xml:"StorageClass"`
}

type s3CommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

type listBucketResultV2 struct {
	XMLName        xml.Name          `xml:"ListBucketResult"`
	XMLNS          string            `xml:"xmlns,attr"`
	Name           string            `xml:"Name"`
	Prefix         string            `xml:"Prefix"`
	Delimiter      string            `xml:"Delimiter,omitempty"`
	KeyCount       int               `xml:"KeyCount"`
	MaxKeys        int               `xml:"MaxKeys"`
	IsTruncated    bool              `xml:"IsTruncated"`
	Contents       []s3Object        `xml:"Contents"`
	CommonPrefixes []s3CommonPrefix  `xml:"CommonPrefixes,omitempty"`
}

// ----- Multipart XML (for clients that use it instead of JSON) ------------

type s3CompletedPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeMultipartUploadRequest struct {
	XMLName xml.Name          `xml:"CompleteMultipartUpload"`
	Parts   []s3CompletedPart `xml:"Part"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// listMultipartUploadsResult is the S3 "ListMultipartUploads" wire format.
// We only populate Bucket + Uploads; the pagination fields (KeyMarker,
// UploadIdMarker, IsTruncated, MaxUploads) aren't implemented — the dataset
// is bounded by the 7-day expiry plus per-user filtering, so it stays tiny.
type listMultipartUploadsResult struct {
	XMLName xml.Name      `xml:"ListMultipartUploadsResult"`
	XMLNS   string        `xml:"xmlns,attr"`
	Bucket  string        `xml:"Bucket"`
	Uploads []s3UploadXML `xml:"Upload"`
}

type s3UploadXML struct {
	Key       string `xml:"Key"`
	UploadID  string `xml:"UploadId"`
	Initiated string `xml:"Initiated"` // RFC3339
}
