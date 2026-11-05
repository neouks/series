-- Lossless candidate text, not a stored evidence copy. Keep each searchable
-- field's text semantics aligned with contextNodeFields; exact field matching
-- remains the final verifier (cross-field matches here are only false positives).
CREATE OR REPLACE FUNCTION series_context_search_text_v1(p JSONB) RETURNS TEXT LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT COALESCE(string_agg(lower(body),chr(10)),'') FROM (
 SELECT COALESCE(value,'') body FROM jsonb_each_text(CASE WHEN jsonb_typeof(p)='object' THEN p ELSE '{}'::jsonb END) WHERE key=ANY(ARRAY['summary','name','detail','endpoint','url','phase','claim_key','evidence','text','body'])
 UNION ALL SELECT p#>>'{reproduction,entry_point}' WHERE jsonb_typeof(p#>'{reproduction,entry_point}')='string'
 UNION ALL SELECT x.value#>>'{}' FROM jsonb_array_elements(CASE WHEN jsonb_typeof(p#>'{reproduction,prerequisites}')='array' THEN p#>'{reproduction,prerequisites}' ELSE '[]'::jsonb END)x(value) WHERE jsonb_typeof(x.value)='string'
 UNION ALL SELECT s.value->>f.key FROM jsonb_array_elements(CASE WHEN jsonb_typeof(p#>'{reproduction,steps}')='array' THEN p#>'{reproduction,steps}' ELSE '[]'::jsonb END)s(value)
 CROSS JOIN unnest(ARRAY['title','action','result','proves','request','response','command','output'])f(key)
 WHERE jsonb_typeof(s.value)='object' AND jsonb_typeof(s.value->f.key)='string'
 ) fields
$$;
CREATE OR REPLACE FUNCTION series_context_literal_pattern_v1(q TEXT) RETURNS TEXT LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT '%'||replace(replace(replace(lower(q),chr(92),chr(92)||chr(92)),'%',chr(92)||'%'),'_',chr(92)||'_')||'%'
$$;
