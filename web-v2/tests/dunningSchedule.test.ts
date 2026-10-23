import { describe, expect, it } from 'vitest'
import { parseRetryDays, retryDayNumbers, retryPlanText } from '../src/lib/dunningSchedule'

describe('retry_days', () => {
  it('counts each retry from the one before it', () => {
    expect(retryDayNumbers([3, 4, 7])).toEqual([3, 7, 14])
    expect(retryDayNumbers([3, 1, 3, 7])).toEqual([3, 4, 7, 14])
  })

  it('says how many retries and on which days', () => {
    expect(retryPlanText([3, 4, 7])).toBe('3 retries, on day 3, 7 and 14')
    expect(retryPlanText([2])).toBe('1 retry, on day 2')
    expect(retryPlanText([1, 1])).toBe('2 retries, on day 1 and 2')
  })

  it('reads the editor text as whole days', () => {
    expect(parseRetryDays('3, 4, 7')).toEqual({ days: [3, 4, 7] })
    expect(parseRetryDays(' 30 ,1,')).toEqual({ days: [30, 1] })
  })

  it('refuses what the server refuses', () => {
    for (const bad of ['', ' , ', '0', '31', '1.5', '72h', '-1', '3, x', Array(16).fill('1').join(',')]) {
      expect(parseRetryDays(bad).error, bad).toBeTruthy()
    }
    expect(parseRetryDays(Array(15).fill('1').join(',')).error).toBeUndefined()
  })
})
