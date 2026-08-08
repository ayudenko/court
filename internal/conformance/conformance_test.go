package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"court/internal/core"
	"court/internal/protocol"
)

const (
	tracesDir = "../golden/testdata"
	specPath  = "../../SPEC.md"
)

// TestEveryGoldenTraceConformsToSpec is the direction that makes SPEC.md
// descriptive of the running service rather than of an intention: the checked-in
// traces are produced by core.Service through the same snapshot and producer the
// export endpoint uses, so a rule that the implementation breaks fails here.
func TestEveryGoldenTraceConformsToSpec(t *testing.T) {
	for _, name := range traceNames(t) {
		t.Run(name, func(t *testing.T) {
			if violations := Check(readTrace(t, name)); len(violations) != 0 {
				t.Fatalf("golden trace violates SPEC.md:\n%s", format(violations))
			}
		})
	}
}

// TestConformanceRejectsEachViolation is the other direction: a checker that
// accepts everything would make the test above pass on any artifact. Each case
// mutates a conforming trace in one way and requires exactly that rule to fire.
func TestConformanceRejectsEachViolation(t *testing.T) {
	for _, testCase := range rejectionCases {
		t.Run(testCase.rule+"/"+testCase.name, func(t *testing.T) {
			data := readTrace(t, testCase.trace)
			if len(Check(data)) != 0 {
				t.Fatalf("%s does not conform before mutation", testCase.trace)
			}
			mutated := testCase.mutate(t, data)
			violations := Check(mutated)
			if len(violations) == 0 {
				t.Fatalf("mutation %q was accepted; %s enforces nothing", testCase.name, testCase.rule)
			}
			// Требуется не «правило среди прочих», а точный набор. Иначе случай,
			// который на самом деле ловится соседним правилом, продолжал бы
			// зеленеть после того, как названное правило перестало работать.
			allowed := map[string]bool{testCase.rule: true}
			for _, rule := range testCase.alsoBreaks {
				allowed[rule] = true
			}
			fired := make(map[string]bool, len(violations))
			for _, violation := range violations {
				if !allowed[violation.Rule] {
					t.Fatalf("mutation %q was meant to break %s alone but also broke %s:\n%s",
						testCase.name, testCase.rule, violation.Rule, format(violations))
				}
				fired[violation.Rule] = true
			}
			if !fired[testCase.rule] {
				t.Fatalf("mutation %q broke %s but the checker reported:\n%s",
					testCase.name, testCase.rule, format(violations))
			}
		})
	}
}

// TestEverySpecRuleIsEnforcedAndDocumented closes the loop the conformance
// package exists for. A normative rule that SPEC.md states without a check is
// prose a second implementation cannot be held to; a check without a rule in
// SPEC.md is behaviour nobody agreed to. Both are failures here.
func TestEverySpecRuleIsEnforcedAndDocumented(t *testing.T) {
	registered := make([]string, 0, len(rules))
	for _, rule := range rules {
		registered = append(registered, rule.ID)
	}

	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read SPEC.md: %v", err)
	}
	// Заголовок нормативного правила в SPEC.md — «### C4. Заголовок». Текст
	// заголовка сверяется с реестром здесь же: strings.Contains по всему файлу
	// удовлетворился бы совпадением в таблице, комментарии или другом разделе,
	// и правило C4 могло бы называться в документе чем угодно.
	heading := regexp.MustCompile(`(?m)^### (C\d+)\. (.+)$`)
	titles := make(map[string]string)
	var documented []string
	for _, match := range heading.FindAllStringSubmatch(string(spec), -1) {
		documented = append(documented, match[1])
		titles[match[1]] = strings.TrimSpace(match[2])
	}

	tested := make([]string, 0, len(rejectionCases))
	for _, testCase := range rejectionCases {
		tested = append(tested, testCase.rule)
	}

	// Дубликаты проверяются до схлопывания: два заголовка «### C11.» с разным
	// текстом или две записи реестра с одним идентификатором прошли бы сравнение
	// множеств незамеченными, а расходятся при этом ровно те смыслы, которые этот
	// тест и должен удерживать вместе. Несколько тестов на одно правило —
	// наоборот, норма, поэтому tested не проверяется на дубликаты.
	requireNoDuplicates(t, "SPEC.md rule headings", documented)
	requireNoDuplicates(t, "checker rule registry", registered)

	if got, want := sortedUnique(documented), sortedUnique(registered); !equal(got, want) {
		t.Errorf("SPEC.md documents rules %v, the checker registers %v", got, want)
	}
	if got, want := sortedUnique(tested), sortedUnique(registered); !equal(got, want) {
		t.Errorf("rejection tests cover rules %v, the checker registers %v", got, want)
	}
	for _, rule := range rules {
		if titles[rule.ID] != rule.Title {
			t.Errorf("SPEC.md heads rule %s as %q, the checker registers %q",
				rule.ID, titles[rule.ID], rule.Title)
		}
	}
}

// TestSpecStatesTheLimitsItIsCheckedAgainst pins the numbers SPEC.md publishes to
// the constants the service enforces. Rule identifiers can be reconciled while a
// bound drifts: raising core.MaxParticipants looks like a service change, but it
// silently widens a published rule and leaves a second implementation rejecting
// artifacts this one calls conforming.
func TestSpecStatesTheLimitsItIsCheckedAgainst(t *testing.T) {
	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read SPEC.md: %v", err)
	}
	for _, limit := range []struct {
		what     string
		sentence string
	}{
		{"participant maximum", fmt.Sprintf("Participants are between 2 and %d (C1)", core.MaxParticipants)},
		{"turn timeout range", fmt.Sprintf("`turn_timeout_sec` is between %d and %d, default %d",
			core.MinTurnTimeout, core.MaxTurnTimeout, core.DefaultTimeoutSec)},
		{"preparation range", fmt.Sprintf("`prep_time_sec` is between 0 and %d", core.MaxPrepTime)},
		{"round range", fmt.Sprintf("`rounds` is between 1 and %d, default %d", core.MaxRounds, core.DefaultRounds)},
		{"artifact ceiling", fmt.Sprintf("larger than %d MiB", protocol.MaxArtifactBytes>>20)},
	} {
		if !strings.Contains(string(spec), limit.sentence) {
			t.Errorf("SPEC.md does not state the %s as the code enforces it; expected to find %q",
				limit.what, limit.sentence)
		}
	}
}

// TestSpecPublishesTheDegradationNoticesTheServiceEmits pins the degradation
// messages for the same reason as the bounds above. SPEC.md tells a consumer to
// match these strings, because a `system` message carries no structured result —
// so a reworded notice is a silent break of every consumer that followed the
// document. Neither the rule-identifier test nor the golden traces catch it: the
// text is descriptive prose, and only two of the six appear in a trace.
func TestSpecPublishesTheDegradationNoticesTheServiceEmits(t *testing.T) {
	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read SPEC.md: %v", err)
	}
	notices := core.DegradationNotices()
	if len(notices) != 6 {
		t.Fatalf("degradation notices = %d; SPEC.md states there are six", len(notices))
	}
	for _, notice := range notices {
		if !strings.Contains(string(spec), notice) {
			t.Errorf("SPEC.md does not publish the degradation notice %q", notice)
		}
	}
	requireNoDuplicates(t, "degradation notices", notices)
}

// TestCheckRecordsEnforcesTheWholeStream covers the entry point a foreign
// harness reaches for when it decoded the artifact itself. Records that are each
// individually valid can still not form a stream, and answering that with a
// clean report — or with a panic — would make the suite agree with an artifact
// Check rejects on the same bytes.
func TestCheckRecordsEnforcesTheWholeStream(t *testing.T) {
	records, err := protocol.DecodeJSONL(readTrace(t, concludedModerator))
	if err != nil {
		t.Fatalf("DecodeJSONL: %v", err)
	}
	if violations := CheckRecords(records); len(violations) != 0 {
		t.Fatalf("decoded fixture does not conform:\n%s", format(violations))
	}

	for _, testCase := range []struct {
		name    string
		records []protocol.ExportRecord
	}{
		{"a second debate record", append(append([]protocol.ExportRecord(nil), records...), records[0])},
		{"no records at all", nil},
		{"a record with no payload", []protocol.ExportRecord{{
			SchemaVersion: core.CurrentProtocolSchemaVersion,
			RecordType:    protocol.RecordDebate,
			DebateID:      "dbt_empty",
		}}},
		{"a participant record first", records[1:]},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			violations := CheckRecords(testCase.records)
			if len(violations) != 1 || violations[0].Rule != "C0" {
				t.Fatalf("want a single C0 violation, got:\n%s", format(violations))
			}
		})
	}
}

type rejectionCase struct {
	rule  string
	name  string
	trace string
	// alsoBreaks lists rules the mutation unavoidably breaks alongside the one
	// under test. Keeping it explicit is what makes every other case a proof of
	// independent enforcement rather than a coincidence.
	alsoBreaks []string
	mutate     func(*testing.T, []byte) []byte
}

var rejectionCases = []rejectionCase{
	{
		rule: "C0", name: "truncated JSON line", trace: concludedModerator,
		mutate: func(_ *testing.T, data []byte) []byte {
			return append(data, []byte("{\"schema_version\":1,\n")...)
		},
	},
	{
		// Строки переставляются текстом, а не через edit(): edit перекодирует
		// поток и вернул бы канонический порядок, то есть чинил бы ровно то
		// нарушение, которое случай должен предъявить.
		rule: "C0", name: "votes emitted before the transcript", trace: concludedHybrid,
		mutate: func(t *testing.T, data []byte) []byte {
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			last := len(lines) - 1
			lines[last-1], lines[last-2] = lines[last-2], lines[last-1]
			return []byte(strings.Join(lines, "\n") + "\n")
		},
	},
	{
		rule: "C0", name: "a vote duplicated into fake unanimity", trace: runningHybrid,
		mutate: func(t *testing.T, data []byte) []byte {
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			return []byte(strings.Join(append(lines, lines[len(lines)-1]), "\n") + "\n")
		},
	},
	{
		rule: "C0", name: "a record spliced in from another debate", trace: concludedModerator,
		mutate: func(t *testing.T, data []byte) []byte {
			other := readTrace(t, concludedHybrid)
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			foreign := strings.Split(strings.TrimSuffix(string(other), "\n"), "\n")
			return []byte(strings.Join(append(lines, foreign[1]), "\n") + "\n")
		},
	},
	{
		rule: "C1", name: "started debate left with one participant", trace: runningHybrid,
		mutate: dropParticipant(1),
	},
	{
		rule: "C2", name: "argument spoken by a non-participant", trace: concludedModerator,
		mutate: editMessage(0, func(message *protocol.MessageRecord) {
			message.SpeakerID = "agt_not_in_this_debate"
		}),
	},
	{
		// Трасса режима moderator, а не hybrid: там голосов нет, поэтому
		// перенаправленная поддержка нарушает ровно C3 и не задевает C10.
		rule: "C3", name: "vote for a non-participant", trace: concludedModerator,
		mutate: editMessage(0, func(message *protocol.MessageRecord) {
			message.SupportID = "agt_not_in_this_debate"
			message.SupportName = "Outsider"
		}),
	},
	{
		rule: "C4", name: "second participant speaks before the first", trace: concludedModerator,
		mutate: func(t *testing.T, data []byte) []byte {
			return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
				arguments := argumentIndexes(records)
				if len(arguments) < 2 {
					t.Fatalf("fixture has %d arguments, want at least two", len(arguments))
				}
				first, second := records[arguments[0]].Message, records[arguments[1]].Message
				// Меняем местами говорящих, а не строки: канонический порядок всё
				// равно сортирует записи по seq, поэтому нарушить очередь можно
				// только тем, кто говорит, а не тем, где стоит строка.
				first.SpeakerID, second.SpeakerID = second.SpeakerID, first.SpeakerID
				first.SpeakerName, second.SpeakerName = second.SpeakerName, first.SpeakerName
				return records
			})
		},
	},
	{
		rule: "C5", name: "transcript runs ahead of current_round", trace: concludedModerator,
		mutate: editDebate(func(debate *protocol.DebateRecord) { debate.CurrentRound = 0 }),
	},
	{
		// C5 состоит из четырёх утверждений, и одного случая на правило мало:
		// удалённая ветка «раунды не идут назад» пережила бы проверку выше.
		// Раунд двигается назад у вердикта, а не у реплики: реплика, переехавшая
		// в чужой раунд, заодно даёт второй ход того же участника и ломает C4,
		// то есть случай перестал бы показывать ветку, ради которой написан.
		rule: "C5", name: "the transcript moves back a round", trace: multiRoundHybrid,
		mutate: func(t *testing.T, data []byte) []byte {
			return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
				for _, record := range records {
					if record.RecordType == protocol.RecordVerdict {
						record.Verdict.Round--
						return records
					}
				}
				t.Fatal("fixture has no verdict to move")
				return records
			})
		},
	},
	{
		rule: "C5", name: "a record outside the declared rounds", trace: multiRoundHybrid,
		mutate: func(t *testing.T, data []byte) []byte {
			return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
				// Именно последняя реплика: она уезжает в новый раунд одна, и
				// C4 молчит. Любая другая оставила бы в раунде двух говорящих
				// не по очереди, и случай перестал бы изолировать C5.
				arguments := argumentIndexes(records)
				last := records[arguments[len(arguments)-1]].Message
				last.Round = records[0].Debate.Rounds + 1
				return records
			})
		},
	},
	{
		rule: "C5", name: "current_round past the declared rounds", trace: concludedModerator,
		mutate: editDebate(func(debate *protocol.DebateRecord) { debate.CurrentRound = debate.Rounds + 1 }),
	},
	{
		rule: "C6", name: "two structured summaries in one round", trace: concludedModerator,
		mutate: func(t *testing.T, data []byte) []byte {
			return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
				for _, record := range records {
					if record.RecordType != protocol.RecordRoundSummary {
						continue
					}
					// Копия встаёт перед вердиктом, а вердикт сдвигается за неё:
					// иначе мутация нарушала бы заодно C7, и случай перестал бы
					// показывать, что именно ловит C6. Пропуски в seq допустимы.
					duplicate := *record.RoundSummary
					duplicate.Seq = maxSeq(records) + 1
					shiftVerdictAfter(t, records, duplicate.Seq+1)
					copied := record
					copied.RoundSummary = &duplicate
					return append(records, copied)
				}
				t.Fatal("fixture has no round summary to duplicate")
				return records
			})
		},
	},
	{
		rule: "C7", name: "transcript continues after the verdict", trace: concludedModerator,
		mutate: appendSystemMessage("Дописано после вердикта."),
	},
	{
		rule: "C8", name: "debate contradicts its own verdict", trace: concludedModerator,
		mutate: editDebate(func(debate *protocol.DebateRecord) { debate.Consensus = !debate.Consensus }),
	},
	{
		rule: "C9", name: "split votes reported as consensus", trace: concludedHybrid,
		mutate: editDebate(func(debate *protocol.DebateRecord) { debate.Consensus = true }),
	},
	{
		rule: "C10", name: "vote does not follow the speaker's last argument", trace: runningHybrid,
		mutate: func(t *testing.T, data []byte) []byte {
			return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
				participants := participantRecords(records)
				for _, record := range records {
					if record.RecordType != protocol.RecordVote {
						continue
					}
					for _, participant := range participants {
						if participant.AgentID != record.Vote.AgentID {
							record.Vote.SupportsID = participant.AgentID
							record.Vote.SupportsName = participant.Name
							return records
						}
					}
				}
				t.Fatal("fixture has no vote to redirect")
				return records
			})
		},
	},
	{
		// Ветка «проголосовал, но не говорил» — половина C10, отвечающая за
		// подделку: без неё выдуманный голос молчавшего участника проходит.
		rule: "C10", name: "a vote from a participant who never spoke", trace: runningHybrid,
		mutate: func(t *testing.T, data []byte) []byte {
			return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
				silent := silentParticipant(t, records)
				return append(records, protocol.ExportRecord{
					RecordType: protocol.RecordVote,
					DebateID:   records[0].DebateID,
					Vote: &protocol.VoteRecord{
						AgentID: silent.AgentID, AgentName: silent.Name,
						SupportsID: silent.AgentID, SupportsName: silent.Name,
					},
				})
			})
		},
	},
	{
		// Обратная ветка: голос говорившего убран, и артефакт больше не
		// показывает позицию, которую тот занял.
		rule: "C10", name: "a speaker whose vote was dropped", trace: runningHybrid,
		mutate: func(t *testing.T, data []byte) []byte {
			return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
				kept := records[:0:0]
				for _, record := range records {
					if record.RecordType != protocol.RecordVote {
						kept = append(kept, record)
					}
				}
				return kept
			})
		},
	},
	{
		// Подготовка — единственный статус, где C10 не работает (говорить ещё
		// никто не мог), поэтому выдуманный блок голосов держит здесь только C11.
		rule: "C11", name: "votes in a debate that has not started", trace: runningHybrid,
		mutate: func(t *testing.T, data []byte) []byte {
			return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
				records[0].Debate.Status = core.StatusPreparing
				records[0].Debate.CurrentRound = 0
				kept := records[:0:0]
				for _, record := range records {
					switch record.RecordType {
					case protocol.RecordMessage, protocol.RecordRoundSummary, protocol.RecordVerdict:
					default:
						kept = append(kept, record)
					}
				}
				return kept
			})
		},
	},
	{
		rule: "C11", name: "votes in a moderator debate", trace: concludedModerator,
		mutate: func(t *testing.T, data []byte) []byte {
			return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
				participant := participantRecords(records)[0]
				return append(records, protocol.ExportRecord{
					RecordType: protocol.RecordVote,
					DebateID:   records[0].DebateID,
					Vote: &protocol.VoteRecord{
						AgentID: participant.AgentID, AgentName: participant.Name,
						SupportsID: participant.AgentID, SupportsName: participant.Name,
					},
				})
			})
		},
	},
	{
		rule: "C12", name: "verdict cites a record that does not exist", trace: concludedModerator,
		mutate: func(t *testing.T, data []byte) []byte {
			return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
				for _, record := range records {
					if record.RecordType != protocol.RecordVerdict || record.Verdict.Result == nil {
						continue
					}
					claims := record.Verdict.Result.Claims
					if len(claims) == 0 || len(claims[0].Citations) == 0 {
						t.Fatal("fixture verdict has no citation to corrupt")
					}
					claims[0].Citations[0] = maxSeq(records) + 100
					return records
				}
				t.Fatal("fixture has no structured verdict")
				return records
			})
		},
	},
	{
		rule: "C13", name: "context disclosed before the debate starts", trace: openEmbargo,
		mutate: editDebate(func(debate *protocol.DebateRecord) {
			debate.Description = "Материалы, которых присоединившийся видеть ещё не должен."
		}),
	},
	{
		// Реплика до старта неизбежно оказывается и в раунде впереди
		// current_round = 0, поэтому C5 здесь срабатывает по построению.
		rule: "C14", name: "argument recorded before the first round", trace: openEmbargo,
		alsoBreaks: []string{"C5"},
		mutate: func(t *testing.T, data []byte) []byte {
			return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
				participant := participantRecords(records)[0]
				return append(records, protocol.ExportRecord{
					RecordType: protocol.RecordMessage,
					DebateID:   records[0].DebateID,
					Message: &protocol.MessageRecord{
						Seq: 1, Round: 1, SpeakerID: participant.AgentID, SpeakerName: participant.Name,
						Kind: core.KindArgument, Text: "Сказано до старта.",
						CreatedAt: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC),
					},
				})
			})
		},
	},
}

const (
	openEmbargo        = "open_embargo_v1.jsonl"
	runningHybrid      = "hybrid_running_partial_v1.jsonl"
	concludedModerator = "moderator_consensus_v1.jsonl"
	concludedHybrid    = "hybrid_split_vote_v1.jsonl"
	multiRoundHybrid   = "hybrid_multi_round_v1.jsonl"
)

// --- Помощники мутации ---

// edit decodes a trace, hands the records to fn, and re-encodes. Re-encoding
// goes through the production producer, so a mutation that the schema already
// rejects cannot masquerade as a semantic finding.
func edit(t *testing.T, data []byte, fn func([]protocol.ExportRecord) []protocol.ExportRecord) []byte {
	t.Helper()
	records, err := protocol.DecodeJSONL(data)
	if err != nil {
		t.Fatalf("DecodeJSONL: %v", err)
	}
	// Результат мутации переупорядочивается до записи: иначе добавленная запись
	// оседает в конце файла и случай нарушает заодно порядок из C0, то есть
	// перестаёт показывать то правило, ради которого написан. Случаи про сам
	// порядок сюда не идут — они правят строки текстом.
	canonical, err := protocol.Canonicalize(fn(records))
	if err != nil {
		t.Fatalf("Canonicalize after mutation: %v", err)
	}
	mutated, err := protocol.MarshalJSONL(canonical)
	if err != nil {
		t.Fatalf("MarshalJSONL after mutation: %v", err)
	}
	return mutated
}

func editDebate(fn func(*protocol.DebateRecord)) func(*testing.T, []byte) []byte {
	return func(t *testing.T, data []byte) []byte {
		return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
			fn(records[0].Debate)
			return records
		})
	}
}

func editMessage(index int, fn func(*protocol.MessageRecord)) func(*testing.T, []byte) []byte {
	return func(t *testing.T, data []byte) []byte {
		return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
			arguments := argumentIndexes(records)
			if index >= len(arguments) {
				t.Fatalf("fixture has %d arguments, want more than %d", len(arguments), index)
			}
			fn(records[arguments[index]].Message)
			return records
		})
	}
}

func dropParticipant(index int) func(*testing.T, []byte) []byte {
	return func(t *testing.T, data []byte) []byte {
		return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
			seen := -1
			for position, record := range records {
				if record.RecordType != protocol.RecordParticipant {
					continue
				}
				if seen++; seen == index {
					return append(records[:position:position], records[position+1:]...)
				}
			}
			t.Fatalf("fixture has no participant at index %d", index)
			return records
		})
	}
}

func appendSystemMessage(text string) func(*testing.T, []byte) []byte {
	return func(t *testing.T, data []byte) []byte {
		return edit(t, data, func(records []protocol.ExportRecord) []protocol.ExportRecord {
			return append(records, protocol.ExportRecord{
				RecordType: protocol.RecordMessage,
				DebateID:   records[0].DebateID,
				Message: &protocol.MessageRecord{
					Seq: maxSeq(records) + 1, Round: records[0].Debate.CurrentRound,
					SpeakerName: "система", Kind: core.KindSystem, Text: text,
					CreatedAt: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC),
				},
			})
		})
	}
}

func argumentIndexes(records []protocol.ExportRecord) []int {
	var indexes []int
	for position, record := range records {
		if record.RecordType == protocol.RecordMessage && record.Message.Kind == core.KindArgument {
			indexes = append(indexes, position)
		}
	}
	return indexes
}

// silentParticipant returns a participant who has not spoken, which is the only
// one a fabricated vote can be attributed to without also contradicting a
// recorded argument.
func silentParticipant(t *testing.T, records []protocol.ExportRecord) protocol.ParticipantRecord {
	t.Helper()
	spoke := make(map[string]bool)
	for _, record := range records {
		if record.RecordType == protocol.RecordMessage && record.Message.Kind == core.KindArgument {
			spoke[record.Message.SpeakerID] = true
		}
	}
	for _, participant := range participantRecords(records) {
		if !spoke[participant.AgentID] {
			return participant
		}
	}
	t.Fatal("fixture has no silent participant")
	return protocol.ParticipantRecord{}
}

func participantRecords(records []protocol.ExportRecord) []protocol.ParticipantRecord {
	var participants []protocol.ParticipantRecord
	for _, record := range records {
		if record.RecordType == protocol.RecordParticipant {
			participants = append(participants, *record.Participant)
		}
	}
	return participants
}

// shiftVerdictAfter moves the verdict past a record inserted before it, so a
// mutation aimed at one rule does not incidentally break the "verdict is last"
// rule as well.
func shiftVerdictAfter(t *testing.T, records []protocol.ExportRecord, seq int64) {
	t.Helper()
	for _, record := range records {
		if record.RecordType == protocol.RecordVerdict {
			record.Verdict.Seq = seq
			return
		}
	}
	t.Fatal("fixture has no verdict to move")
}

func requireNoDuplicates(t *testing.T, source string, values []string) {
	t.Helper()
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, duplicate := seen[value]; duplicate {
			t.Errorf("%s names %s more than once", source, value)
		}
		seen[value] = struct{}{}
	}
}

func maxSeq(records []protocol.ExportRecord) int64 {
	var highest int64
	for _, record := range records {
		if seq := transcriptSeq(record); seq > highest {
			highest = seq
		}
	}
	return highest
}

// --- Помощники чтения ---

func traceNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(tracesDir)
	if err != nil {
		t.Fatalf("read traces: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".jsonl" {
			names = append(names, entry.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("no golden traces found; conformance would pass vacuously")
	}
	sort.Strings(names)
	return names
}

func readTrace(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(tracesDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

func format(violations []Violation) string {
	lines := make([]string, 0, len(violations))
	for _, violation := range violations {
		lines = append(lines, "  "+violation.String())
	}
	return strings.Join(lines, "\n")
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
