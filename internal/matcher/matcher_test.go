package matcher

import (
	"testing"
	"time"

	"namo/internal/model"
)

func TestMatchFindsUnicodeNormalizedIncludeAny(t *testing.T) {
	result := Match(model.Notice{Title: "cafe\u0301 설계 용역"}, Rule{IncludeAny: []string{"CAFÉ"}})
	if !result.Matched {
		t.Fatal("notice with a normalized include term must match")
	}
	if !hasReason(result.Reasons, ReasonIncludeAny) {
		t.Fatalf("match reasons = %v, want include-any", result.Reasons)
	}
}

func TestMatchRejectsExcludedTermWithReason(t *testing.T) {
	result := Match(model.Notice{Title: "도로 설계 용역 취소"}, Rule{IncludeAny: []string{"설계"}, Exclude: []string{"취소"}})
	if result.Matched {
		t.Fatal("excluded notice must not match")
	}
	if !hasReason(result.Reasons, ReasonExcluded) {
		t.Fatalf("match reasons = %v, want excluded", result.Reasons)
	}
}

func TestMatchAtRequiresPracticalFiltersAndExplainsEachRule(t *testing.T) {
	now := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	notice := model.Notice{
		Category: model.CategoryConstruction, Title: "서울 도로 설계 용역", Agency: "국토교통부", Region: "서울특별시", Amount: 100,
		Deadline: time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC),
	}
	rule := Rule{
		IncludeAny: []string{"도로"}, IncludeAll: []string{"설계", "용역"}, Categories: []model.Category{model.CategoryConstruction},
		Agencies: []string{"국토"}, Regions: []string{"서울"}, MinAmount: int64ptr(99), MaxAmount: int64ptr(101),
		DeadlineWeekdays: []time.Weekday{time.Thursday}, DeadlineWithinDays: intPtr(3),
	}
	result := MatchAt(now, notice, rule)
	if !result.Matched {
		t.Fatalf("qualified notice did not match: %v", result.Reasons)
	}
	for _, reason := range []Reason{ReasonIncludeAny, ReasonIncludeAll, ReasonCategory, ReasonAgency, ReasonRegion, ReasonMinAmount, ReasonMaxAmount, ReasonDeadlineWeekday, ReasonDeadlineWithinDays} {
		if !hasReason(result.Reasons, reason) {
			t.Fatalf("match reasons = %v, missing %s", result.Reasons, reason)
		}
	}
}

func TestMatchKeywordsInspectAgencyAndRegionWithDetails(t *testing.T) {
	result := Match(model.Notice{Title: "일반 용역", Agency: "국토교통부", Region: "서울특별시"}, Rule{IncludeAny: []string{"국토"}})
	if !result.Matched || len(result.Details) != 1 || result.Details[0] != (Detail{Code: ReasonIncludeAny, Field: "agency", RuleValue: "국토", NoticeValue: "국토교통부"}) {
		t.Fatalf("result = %+v", result)
	}
	excluded := Match(model.Notice{Title: "일반 용역", Region: "제주특별자치도"}, Rule{Exclude: []string{"제주"}})
	if excluded.Matched || len(excluded.Details) != 1 || excluded.Details[0] != (Detail{Code: ReasonExcluded, Field: "region", RuleValue: "제주", NoticeValue: "제주특별자치도"}) {
		t.Fatalf("excluded result = %+v", excluded)
	}
}

func TestMatchAgencyAndRegionDetailsKeepRuleTermsAndNoticeValues(t *testing.T) {
	notice := model.Notice{Agency: "국토교통부", Region: "서울특별시"}
	result := Match(notice, Rule{Agencies: []string{" 해양 ", "국토"}, Regions: []string{"서울"}})
	if !result.Matched || len(result.Details) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if got, want := result.Details[0], (Detail{Code: ReasonAgency, Field: "agency", RuleValue: "국토", NoticeValue: "국토교통부"}); got != want {
		t.Fatalf("agency detail = %+v, want %+v", got, want)
	}
	if got, want := result.Details[1], (Detail{Code: ReasonRegion, Field: "region", RuleValue: "서울", NoticeValue: "서울특별시"}); got != want {
		t.Fatalf("region detail = %+v, want %+v", got, want)
	}
}

func TestMatchInvalidDeadlineDetailsKeepApplicableRuleValues(t *testing.T) {
	weekday := MatchAt(time.Now(), model.Notice{}, Rule{DeadlineWeekdays: []time.Weekday{time.Monday, time.Wednesday}})
	if len(weekday.Details) != 1 || weekday.Details[0] != (Detail{Code: ReasonInvalidDeadline, Field: "deadline", RuleValue: "Monday,Wednesday", NoticeValue: ""}) {
		t.Fatalf("weekday result = %+v", weekday)
	}
	days := 3
	window := MatchAt(time.Now(), model.Notice{}, Rule{DeadlineWithinDays: &days})
	if len(window.Details) != 1 || window.Details[0] != (Detail{Code: ReasonInvalidDeadline, Field: "deadline", RuleValue: "3", NoticeValue: ""}) {
		t.Fatalf("window result = %+v", window)
	}
}

func TestMatchAtRejectsInvalidDeadlineAndAmountBoundary(t *testing.T) {
	now := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	deadline := 3
	min := int64(100)
	if result := MatchAt(now, model.Notice{Amount: 100, Deadline: now.Add(72 * time.Hour)}, Rule{MinAmount: &min, DeadlineWithinDays: &deadline}); !result.Matched {
		t.Fatalf("exact boundaries must match: %+v", result)
	}
	if result := MatchAt(now, model.Notice{Amount: 99, Deadline: now.Add(72 * time.Hour)}, Rule{MinAmount: &min, DeadlineWithinDays: &deadline}); result.Matched {
		t.Fatal("amount below minimum must reject")
	}
	if result := MatchAt(now, model.Notice{}, Rule{DeadlineWithinDays: &deadline}); result.Matched || !hasReason(result.Reasons, ReasonInvalidDeadline) {
		t.Fatalf("zero deadline result = %+v", result)
	}
}

func TestUnlimitedDeadlineRuleMatchesNoticeWithoutDeadline(t *testing.T) {
	now := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	result := MatchAt(now, model.Notice{Title: "데이터 구축 용역"}, Rule{IncludeAny: []string{"데이터"}})
	if !result.Matched {
		t.Fatalf("notice without deadline was dropped: %+v", result)
	}
}

func TestDeadlineWindowRuleDropsNoticeWithoutDeadline(t *testing.T) {
	now := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	days := 3
	result := MatchAt(now, model.Notice{Title: "데이터 구축 용역"}, Rule{IncludeAny: []string{"데이터"}, DeadlineWithinDays: &days})
	if result.Matched {
		t.Fatal("notice without deadline slipped through a deadline window rule")
	}
	if len(result.Reasons) != 1 || result.Reasons[0] != ReasonInvalidDeadline {
		t.Fatalf("unexpected reasons: %+v", result.Reasons)
	}
}

func hasReason(reasons []Reason, want Reason) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func int64ptr(value int64) *int64 { return &value }
func intPtr(value int) *int       { return &value }
