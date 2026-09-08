ALTER TABLE public.report_items ADD COLUMN source_kind text;
ALTER TABLE public.report_items ADD COLUMN posted_at timestamptz;
ALTER TABLE public.report_items ADD COLUMN collected_at timestamptz;
ALTER TABLE public.report_items ADD COLUMN recorded_at timestamptz;

GRANT SELECT, INSERT ON TABLE public.report_items TO namo_runtime;
