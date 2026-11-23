-- Who a push release is credited to: the commit author's name and GitHub
-- login (head_commit.author). The avatar URL is derived from the login at
-- render time. '' = not known (a release cut before this, any non-push kind).
ALTER TABLE releases ADD COLUMN author_name TEXT NOT NULL DEFAULT '';
ALTER TABLE releases ADD COLUMN author_login TEXT NOT NULL DEFAULT '';
