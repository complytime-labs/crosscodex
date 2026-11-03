package llmclient

import (
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"
)

// messagesError is an ErrInvalidMessages whose text is only the detail, so a
// caller can wrap it in its own sentinel without the text growing a suffix.
type messagesError struct{ detail string }

func (e *messagesError) Error() string        { return e.detail }
func (e *messagesError) Is(target error) bool { return target == ErrInvalidMessages }

// MessagesFromPayload decodes the "messages" value of a task payload into
// chat messages. It is the single decoder the worker (to build the request)
// and the analyzers' cache identity (to hash that request) both use, so the
// hash covers exactly what is sent: a field read here is a field hashed by
// ContentHash, and a field not read here is not sent.
//
// v must be a list of structs, each with a non-empty role of RoleSystem,
// RoleUser or RoleAssistant; content is read as a string (absent or
// non-string reads as empty) and other fields are ignored. Order is kept.
// Anything else returns an error matching ErrInvalidMessages whose text names
// the offending element and what to change.
func MessagesFromPayload(v *structpb.Value) ([]ChatMessage, error) {
	list := v.GetListValue()
	if list == nil {
		return nil, &messagesError{"field 'messages' must be a list"}
	}
	messages := make([]ChatMessage, 0, len(list.Values))
	for i, item := range list.Values {
		m := item.GetStructValue()
		if m == nil {
			return nil, &messagesError{fmt.Sprintf("messages[%d] must be a struct", i)}
		}
		role := m.Fields["role"].GetStringValue()
		if role == "" {
			return nil, &messagesError{fmt.Sprintf("messages[%d] missing required field 'role'", i)}
		}
		if role != RoleSystem && role != RoleUser && role != RoleAssistant {
			return nil, &messagesError{fmt.Sprintf("messages[%d] has invalid role %q: must be one of system, user, assistant", i, role)}
		}
		messages = append(messages, ChatMessage{Role: role, Content: m.Fields["content"].GetStringValue()})
	}
	return messages, nil
}
