Read the Jira item `${inputs.jira}` using the Jira connector tools available to you.

Produce the requirements for it in EARS form ("WHEN <trigger> THE SYSTEM SHALL <response>").
Give every requirement a durable ID (REQ-<ticket>-<nn>), a priority (must/should/could),
and list any clarification you need before design can start. Do not start designing.

Write the requirements to `specs/${inputs.jira}/requirements.md` as well as returning them
as structured output.
