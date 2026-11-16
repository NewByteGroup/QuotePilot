// Token the contract mock accepts on POST /auth/session. Kept apart from the handlers so
// the login page can link to it without bundling MSW.
export const MOCK_LOGIN_TOKEN = 'mock-login-token'
