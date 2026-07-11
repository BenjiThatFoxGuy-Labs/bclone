// Package api contains definitions for e621's (and compatible e621ng
// instances') JSON API — see https://e621.net/wiki_pages/2425 for the
// upstream documentation this is modelled on.
package api

// Post is a single post as returned by GET /posts.json.
type Post struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	File      File   `json:"file"`
	Rating    string `json:"rating"`
}

// File describes the actual media backing a Post. URL is omitted by the
// API (left as "") when the post's file isn't visible to the requesting
// user, e.g. a deleted post without the right permissions to view it.
type File struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Ext    string `json:"ext"`
	Size   int64  `json:"size"`
	MD5    string `json:"md5"`
	URL    string `json:"url"`
}

// PostsResponse is the top-level response from GET /posts.json.
type PostsResponse struct {
	Posts []Post `json:"posts"`
}
