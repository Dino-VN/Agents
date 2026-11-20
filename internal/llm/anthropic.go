package llm

import (
	"context"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type Anthropic struct {
	client anthropic.Client
}

// NewAnthropic builds a client for the Anthropic API or an Anthropic-compatible
// endpoint. auth "bearer" sends the key as Authorization: Bearer (common for proxies).
func NewAnthropic(apiKey, baseURL, auth string) *Anthropic {
	var opts []option.RequestOption
	if apiKey != "" {
		if auth == "bearer" {
			opts = append(opts, option.WithAuthToken(apiKey))
		} else {
			opts = append(opts, option.WithAPIKey(apiKey))
		}
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &Anthropic{client: anthropic.NewClient(opts...)}
}

type anthropicSession struct {
	p     *Anthropic
	spec  Spec
	tools []anthropic.ToolUnionParam
	msgs  []anthropic.MessageParam
}

func (p *Anthropic) NewSession(spec Spec, seed []Message) Session {
	s := &anthropicSession{p: p, spec: spec}
	for _, t := range spec.Tools {
		tp := anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: t.Properties, Required: t.Required, ExtraFields: t.Extra},
			// Large inputs (file contents) stream as generated; the runtime validates them.
			EagerInputStreaming: anthropic.Bool(true),
		}
		s.tools = append(s.tools, anthropic.ToolUnionParam{OfTool: &tp})
	}
	for _, m := range normalizeSeed(seed) {
		if m.Role == "user" {
			s.msgs = append(s.msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Text)))
		} else {
			s.msgs = append(s.msgs, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Text)))
		}
	}
	return s
}

func (s *anthropicSession) Len() int { return len(s.msgs) }

func (s *anthropicSession) Send(ctx context.Context, blocks []Block) (*Turn, error) {
	var content []anthropic.ContentBlockParamUnion
	for _, b := range blocks {
		switch b.Type {
		case Text:
			content = append(content, anthropic.NewTextBlock(b.Text))
		case Image:
			content = append(content, anthropic.NewImageBlockBase64(b.MediaType, b.Data))
		case Document:
			content = append(content, anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{Data: b.Data}))
		case ToolResult:
			if len(b.Parts) == 0 {
				content = append(content, anthropic.NewToolResultBlock(b.ToolID, b.Text, b.IsError))
				continue
			}
			tr := anthropic.ToolResultBlockParam{ToolUseID: b.ToolID, IsError: anthropic.Bool(b.IsError)}
			if b.Text != "" {
				tr.Content = append(tr.Content, anthropic.ToolResultBlockParamContentUnion{OfText: &anthropic.TextBlockParam{Text: b.Text}})
			}
			for _, p := range b.Parts {
				img := anthropic.NewImageBlockBase64(p.MediaType, p.Data).OfImage
				tr.Content = append(tr.Content, anthropic.ToolResultBlockParamContentUnion{OfImage: img})
			}
			content = append(content, anthropic.ContentBlockParamUnion{OfToolResult: &tr})
		}
	}
	s.msgs = append(s.msgs, anthropic.NewUserMessage(content...))

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(s.spec.Model.ID),
		MaxTokens: s.spec.MaxTokens,
		System: []anthropic.TextBlockParam{{
			Text:         s.spec.System,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages:     s.msgs,
		Tools:        s.tools,
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
	}
	if s.spec.Effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(s.spec.Effort)}
	}
	var opts []option.RequestOption
	if s.spec.Model.Fallback {
		opts = append(opts,
			option.WithHeaderAdd("anthropic-beta", "server-side-fallback-2026-07-01"),
			option.WithJSONSet("fallbacks", "default"))
	}

	stream := s.p.client.Messages.NewStreaming(ctx, params, opts...)
	msg := anthropic.Message{}
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			s.msgs = s.msgs[:len(s.msgs)-1]
			return nil, err
		}
	}
	if err := stream.Err(); err != nil {
		s.msgs = s.msgs[:len(s.msgs)-1] // keep history valid so the caller can retry
		return nil, err
	}
	s.msgs = append(s.msgs, msg.ToParam())

	t := &Turn{
		Stop:  string(msg.StopReason),
		Usage: Usage{In: msg.Usage.InputTokens + msg.Usage.CacheReadInputTokens + msg.Usage.CacheCreationInputTokens, Out: msg.Usage.OutputTokens},
	}
	var text []string
	for _, block := range msg.Content {
		switch v := block.AsAny().(type) {
		case anthropic.TextBlock:
			text = append(text, v.Text)
		case anthropic.ToolUseBlock:
			t.Calls = append(t.Calls, Block{Type: ToolUse, ToolID: v.ID, ToolName: v.Name, Input: []byte(v.JSON.Input.Raw())})
		}
	}
	t.Text = strings.TrimSpace(strings.Join(text, "\n"))
	return t, nil
}
