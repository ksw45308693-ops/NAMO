ALTER TABLE public.reports DROP CONSTRAINT reports_deleted_only_generated;
ALTER TABLE public.reports ADD CONSTRAINT reports_deleted_only_terminal
    CHECK (deleted_at IS NULL OR status IN ('generated', 'failed'));
