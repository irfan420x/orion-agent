// Package core — screenshot history eviction (computer-use M1,
// plan §4.7-3).
//
// Every screenshot costs ~1,000–1,700 vision tokens, and the cost
// compounds: each image stays in the request history and is
// re-sent on every later step. So the provider-facing history keeps
// at most the last MaxScreenshotsInHistory screenshot images;
// earlier ones are replaced by a text summary — summarized eviction,
// never a FIFO drop that would leave a tool result the model cannot
// understand.
//
// Two places enforce it:
//   - evictOldScreenshotImages, applied to the request copy before
//     every provider call in run(): the in-flight transcript keeps
//     its images, only the request is evicted.
//   - stripImagesForStorage, applied in recordMessage before a
//     message is persisted: the session file store rewrites the
//     whole JSON file on every AddMessage, so persisting
//     multi-hundred-KB images would make every later message
//     rewrite megabytes. Stored history keeps the JSON summary
//     text, never the bytes.
package core

import (
	"fmt"

	"github.com/irfan/orion/internal/model"
)

// MaxScreenshotsInHistory is the most screenshot images the
// provider-facing history may carry (plan §4.7-3).
const MaxScreenshotsInHistory = 3

// evictOldScreenshotImages returns a copy of msgs in which at most
// the last MaxScreenshotsInHistory image-bearing messages keep
// their images. Earlier image-bearing messages keep their text and
// gain an eviction note; their images are dropped. Messages without
// images pass through unchanged (shared, not copied). The input
// slice is never modified.
func evictOldScreenshotImages(msgs []model.Message) []model.Message {
	// Find the image-bearing messages, oldest first.
	var withImages []int
	for i, m := range msgs {
		if len(m.Images) > 0 {
			withImages = append(withImages, i)
		}
	}
	if len(withImages) <= MaxScreenshotsInHistory {
		return msgs
	}
	evict := len(withImages) - MaxScreenshotsInHistory
	evictSet := map[int]bool{}
	for _, i := range withImages[:evict] {
		evictSet[i] = true
	}
	out := make([]model.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if !evictSet[i] {
			continue
		}
		m := out[i]
		m.Images = nil
		m.Content += fmt.Sprintf("\n[screenshot image evicted from history: only the latest %d screenshots are kept; the JSON summary above still describes this capture]", MaxScreenshotsInHistory)
		out[i] = m
	}
	return out
}

// stripImagesForStorage returns msg as it should be persisted: the
// images removed, with a note recording that they were there. A
// message without images is returned unchanged.
func stripImagesForStorage(msg model.Message) model.Message {
	if len(msg.Images) == 0 {
		return msg
	}
	n := len(msg.Images)
	msg.Images = nil
	msg.Content += fmt.Sprintf("\n[screenshot image not stored: %d image(s) removed from the persisted history to keep the session file small; the JSON summary above still describes this capture]", n)
	return msg
}
