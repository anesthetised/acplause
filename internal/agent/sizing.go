package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"

	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/media"
)

// Image budgeting from ADR 0009. The reserve is an explicit heuristic, not a
// provider token count. Each message with images also gets an allowance for
// the label and synthetic message the wire projection adds, or for an
// omission marker.
const (
	imageTokens     = 8192
	imageLabelBytes = 256
)

// requestSize deliberately overestimates tokens without a model-specific
// tokenizer: one serialized byte per token, plus framing (ADR 0006). Session
// estimates calibrate this with provider usage; see calibration.
func requestSize(req llm.Request) uint64 {
	text, images := requestParts(req)
	if text == math.MaxUint64 {
		return text
	}
	return text + images
}

// requestParts splits the estimate into serialized text bytes with framing
// and the image reserve, which is already in tokens. Image bytes are never
// counted as text; images the model will see are reserved.
func requestParts(req llm.Request) (text, images uint64) {
	messages := make([]llm.Message, len(req.Messages))
	for i, m := range req.Messages {
		if len(m.Images) > 0 {
			images += imageLabelBytes
			if req.Vision {
				images += imageTokens * uint64(len(m.Images))
			}
			m.Images = nil
		}
		messages[i] = m
	}
	data, err := json.Marshal(struct {
		Messages []llm.Message `json:"messages"`
		Tools    []llm.Tool    `json:"tools,omitempty"`
	}{messages, req.Tools})
	if err != nil {
		return math.MaxUint64, images // invalid input must never be considered to fit
	}
	return uint64(len(data)) + uint64(len(req.Messages))*16 + 256, images
}

// byteCounter counts what is written to it.
type byteCounter uint64

func (c *byteCounter) Write(p []byte) (int, error) {
	*c += byteCounter(len(p))
	return len(p), nil
}

// jsonLen is len(json.Marshal(v)) without keeping the encoding.
func jsonLen(v any) (uint64, error) {
	var n byteCounter
	if err := json.NewEncoder(&n).Encode(v); err != nil {
		return 0, err
	}
	return uint64(n) - 1, nil // Encode ends the value with a newline
}

// Pieces of requestParts' body, {"messages":[m1,m2],"tools":[...]}: the braces
// and brackets around the messages, the comma between two of them, and the
// key that introduces tools.
const (
	bodyFraming    = len(`{"messages":[]}`)
	toolsKeyBytes  = len(`,"tools":`)
	messageFraming = 16 // per message, ADR 0006
)

// messageCost is what one message adds to requestParts.
type messageCost struct {
	text, images uint64
	bad          bool // does not serialize, so the request never fits
}

// costOf measures a message as requestParts does, including its comma.
func costOf(m llm.Message, vision bool) messageCost {
	var c messageCost
	if len(m.Images) > 0 {
		c.images = imageLabelBytes
		if vision {
			c.images += imageTokens * uint64(len(m.Images))
		}
		m.Images = nil
	}
	// As an element of a slice, which is how requestParts and the summary
	// encode it: the brackets stand in for the comma.
	n, err := jsonLen([]llm.Message{m})
	c.text, c.bad = n-2+messageFraming+1, err != nil
	return c
}

// requestSizer keeps requestParts for a message list that changes one message
// at a time, and for its image projection (projectImages), so that neither the
// trimming loop nor an image removal has to serialize the whole request again.
// The total is the sum of per-message sizes plus a constant; a test holds it
// equal to requestParts. A nil sizer is valid and tracks nothing, for requests
// whose size is never read. The caller must keep it in step with the messages.
type requestSizer struct {
	vision       bool
	fixed        uint64 // framing and tools
	bad          int    // tools or messages that do not serialize
	text, images uint64 // sums over msgs, as projected
	msgs         []sizedMessage
}

type sizedMessage struct {
	cost    messageCost // as in the request
	omitted *omission   // built when its images are first dropped
	dropped bool        // the projection uses omitted
}

// omission is a message with its images replaced by a marker.
type omission struct {
	msg  llm.Message
	cost messageCost
}

func newRequestSizer(req llm.Request) *requestSizer {
	z := &requestSizer{vision: req.Vision, fixed: uint64(bodyFraming) + 256, msgs: make([]sizedMessage, len(req.Messages))}
	if len(req.Tools) > 0 {
		if n, err := jsonLen(req.Tools); err == nil {
			z.fixed += uint64(toolsKeyBytes) + n
		} else {
			z.bad++
		}
	}
	for i, m := range req.Messages {
		z.msgs[i].cost = costOf(m, z.vision)
		z.add(z.msgs[i].cost)
	}
	return z
}

func (z *requestSizer) add(c messageCost) {
	z.text += c.text
	z.images += c.images
	if c.bad {
		z.bad++
	}
}

func (z *requestSizer) sub(c messageCost) {
	z.text -= c.text
	z.images -= c.images
	if c.bad {
		z.bad--
	}
}

// current is the cost message i has in the projection.
func (z *requestSizer) current(i int) messageCost {
	if m := &z.msgs[i]; m.dropped {
		return m.omitted.cost
	}
	return z.msgs[i].cost
}

// set replaces message i, whose images are back, with m.
func (z *requestSizer) set(i int, m llm.Message) {
	if z == nil {
		return
	}
	z.sub(z.current(i))
	z.msgs[i] = sizedMessage{cost: costOf(m, z.vision)}
	z.add(z.msgs[i].cost)
}

// reset restores every message's images, starting a projection over.
func (z *requestSizer) reset() {
	if z == nil {
		return
	}
	for i := range z.msgs {
		if z.msgs[i].dropped {
			z.sub(z.current(i))
			z.msgs[i].dropped = false
			z.add(z.msgs[i].cost)
		}
	}
}

// dropImages projects message i, which must be the one the sizer holds, to
// omitImages(m, reason). The result is built once per message and reason.
func (z *requestSizer) dropImages(i int, m llm.Message, reason string) llm.Message {
	if z == nil {
		return omitImages(m, reason)
	}
	e := &z.msgs[i]
	if e.omitted == nil {
		o := omitImages(m, reason)
		e.omitted = &omission{o, costOf(o, z.vision)}
	}
	if !e.dropped {
		z.sub(e.cost)
		e.dropped = true
		z.add(e.omitted.cost)
	}
	return e.omitted.msg
}

// size is calibration.size of the projected request.
func (z *requestSizer) size(c *calibration, model string) uint64 {
	if z.bad > 0 {
		return math.MaxUint64 // invalid input must never be considered to fit
	}
	text := z.fixed + z.text
	if len(z.msgs) > 0 {
		text-- // one comma fewer than messages
	}
	return c.estimate(model, text, z.images)
}

// projectImages applies the request image policy to a copy of the messages;
// history and replay keep every image. A model not known to see images gets
// markers instead. Otherwise the oldest unprotected images are replaced by
// markers, a whole message at a time, until the request is within the image
// limits and fits. Protected images over the limits are an error; protected
// content over the window is left to text trimming and compaction.
//
// z follows the projection, and fits reads it.
func projectImages(req llm.Request, z *requestSizer, fits func() bool) (llm.Request, error) {
	z.reset()
	req.Messages = slices.Clone(req.Messages)
	messages := req.Messages
	if !req.Vision {
		for i, m := range messages {
			if len(m.Images) > 0 {
				messages[i] = z.dropImages(i, m, "the selected model does not accept images")
			}
		}
		return req, nil
	}
	count, size := 0, 0
	for _, m := range messages {
		count += len(m.Images)
		size += imageBytes(m.Images)
	}
	within := func() bool { return count <= media.MaxImages && size <= media.MaxSetBytes }
	user, keep := latest(messages)
	for i := 0; i < keep && !(within() && fits()); i++ {
		if i == user || len(messages[i].Images) == 0 {
			continue
		}
		count -= len(messages[i].Images)
		size -= imageBytes(messages[i].Images)
		messages[i] = z.dropImages(i, messages[i], "removed from this request to fit the model's limits")
	}
	if !within() {
		return req, fmt.Errorf("the latest prompt and tool results carry %d images of %d bytes; one model request allows %d images of %d bytes",
			count, size, media.MaxImages, media.MaxSetBytes)
	}
	return req, nil
}

func imageBytes(images []media.Image) int {
	n := 0
	for _, img := range images {
		n += img.Size()
	}
	return n
}

// omitImages keeps a message's text and states that its images are absent,
// so the model does not mistake a gap for an empty result.
func omitImages(m llm.Message, reason string) llm.Message {
	marker := fmt.Sprintf("[%d images omitted: %s]", len(m.Images), reason)
	if len(m.Images) == 1 {
		marker = "[image omitted: " + reason + "]"
	}
	m.Content = joinLines(m.Content, marker)
	m.Images = nil
	return m
}

// historyJSON keeps the serialization of history messages, and the bytes each
// takes once embedded in a JSON string, while a summary chunk grows over them.
// A message is serialized once however many candidates contain it.
type historyJSON struct {
	data    [][]byte
	escaped []uint64
}

// extend serializes messages up to n.
func (h *historyJSON) extend(messages []llm.Message, n int) error {
	for len(h.data) < n {
		data, err := json.Marshal(messages[len(h.data) : len(h.data)+1])
		if err != nil {
			return err
		}
		data = data[1 : len(data)-1] // the element, as in the array of the whole chunk
		quoted, err := jsonLen(string(data))
		if err != nil {
			return err
		}
		h.data = append(h.data, data)
		h.escaped = append(h.escaped, quoted-2) // without the quotes
	}
	return nil
}

// drop forgets the first n messages once they are summarized.
func (h *historyJSON) drop(n int) {
	clear(h.data[:n])
	h.data, h.escaped = h.data[n:], h.escaped[n:]
}
