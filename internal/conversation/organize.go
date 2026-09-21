package conversation

import "slices"

// lastPairedIndex finds the most recent partner for a message. It searches backwards so a result binds
// to the call that just happened, not an earlier matching one.
func lastPairedIndex(messages []Message, message Message) int {
	for index, candidate := range slices.Backward(messages) {
		if candidate.Activity.IsPair(message.Activity) {
			return index
		}
	}

	return -1
}

// clusterActivities moves each tool result to sit directly after its call. Several providers validate that
// a tool message immediately follows the assistant turn that requested it, which a rebuilt history does not guarantee.
func clusterActivities(messages []Message) []Message {
	organized := make([]Message, 0, len(messages))

	for index, message := range messages {
		if message.Type != TypeActivity {
			organized = append(organized, message)

			continue
		}

		activity := message.Activity

		if activity == nil || activity.Kind == "" {
			// an activity message with no activity in it describes nothing. It
			// cannot be paired, rendered, or acted on
			continue
		}

		switch activity.Kind {
		case ActivityRequest:
			organized = append(organized, message)

		case ActivityResponse:
			partner := lastPairedIndex(organized, message)

			if partner < 0 {
				// the call it answers is gone - dropped here rather than sent as
				// a result to nothing
				continue
			}

			organized = slices.Insert(organized, partner+1, message)

		case ActivityTrigger:
			// a trigger says "act now". Anywhere but last it is describing a
			// moment that has already passed
			if slices.ContainsFunc(messages[index+1:], func(later Message) bool { return later.Type != TypeInstructions }) {
				continue
			}

			organized = append(organized, message)

		default:
			// an unrecognized activity kind is not something a provider can be
			// asked to interpret
			continue
		}
	}

	return organized
}

// dropOrphanedActivities removes halves of a pair whose partner is missing, in both directions, since
// trimming can take either end of a pair.
func dropOrphanedActivities(messages []Message) []Message {
	kept := make([]Message, 0, len(messages))

	for _, message := range messages {
		if message.Type != TypeActivity || message.Activity == nil {
			kept = append(kept, message)

			continue
		}

		halfOfAPair := message.Activity.Kind == ActivityRequest || message.Activity.Kind == ActivityResponse

		if halfOfAPair && !slices.ContainsFunc(messages, func(candidate Message) bool { return candidate.Activity.IsPair(message.Activity) }) {
			continue
		}

		kept = append(kept, message)
	}

	return kept
}

// dropEmpty removes messages that carry nothing, which cost tokens and tell the model nothing. The system
// prompt is exempt, since dropping an empty one would change which message comes first.
func dropEmpty(messages []Message) []Message {
	kept := make([]Message, 0, len(messages))

	for _, message := range messages {
		if message.Text == "" && message.Activity == nil && message.Type != TypeInstructions {
			continue
		}

		kept = append(kept, message)
	}

	return kept
}

// sameMessage compares two messages for the duplicate check.
func sameMessage(a, b Message) bool {
	if a.Type != b.Type || a.Text != b.Text {
		return false
	}

	// Activities are never duplicates of one another. Two calls with the same arguments are two real
	// calls, and collapsing them would hide the repetition the cycle guards look for.
	if a.Type == TypeActivity {
		return false
	}

	return a.Activity == nil && b.Activity == nil
}

// dropConsecutiveDuplicates collapses a message repeated back to back. A retried turn or re-injected notice
// can land twice, and repetition is what the model imitates, feeding the loop the cycle guards catch.
func dropConsecutiveDuplicates(messages []Message) []Message {
	kept := make([]Message, 0, len(messages))

	for index, message := range messages {
		if index > 0 && sameMessage(messages[index-1], message) {
			continue
		}

		kept = append(kept, message)
	}

	return kept
}

// Message hygiene applied before a conversation goes on the wire. A history does not stay tidy on its own, and
// providers reject a whole request for an unpaired call, a stray trigger or an empty message. Organize is a
// pure function over the list, so the engine's history stays the record of what happened.

// Organize repairs a conversation so a provider will accept it. Pairs are clustered first, because the orphan
// checks ask whether a partner exists anywhere, and a moved response is no longer an orphan.
func Organize(messages []Message) []Message {
	organized := clusterActivities(messages)
	organized = dropOrphanedActivities(organized)
	organized = dropEmpty(organized)
	organized = dropConsecutiveDuplicates(organized)

	return organized
}
