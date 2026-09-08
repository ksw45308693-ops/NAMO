ALTER TABLE public.reports ADD COLUMN deleted_at timestamptz;
ALTER TABLE public.reports ADD CONSTRAINT reports_deleted_only_generated
    CHECK (deleted_at IS NULL OR status = 'generated');

GRANT DELETE ON TABLE public.report_items TO namo_runtime;
