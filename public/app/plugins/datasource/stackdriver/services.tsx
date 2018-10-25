import React, { SFC } from 'react';
import uniqBy from 'lodash/uniqBy';

interface Props {
  onServiceChange: any;
  metricDescriptors: any[];
}

const Services: SFC<Props> = props => {
  const extractServices = () =>
    uniqBy(props.metricDescriptors, 'service').map(m => ({
      value: m.service,
      name: m.serviceShortName,
    }));

  return (
    <div className="gf-form max-width-21">
      <span className="gf-form-label width-7">Service</span>
      <div className="gf-form-select-wrapper max-width-14">
        <select className="gf-form-input" required onChange={props.onServiceChange}>
          {extractServices().map((qt, i) => (
            <option key={i} value={qt.value} ng-if="false">
              {qt.name}
            </option>
          ))}
        </select>
      </div>
    </div>
  );
};

export default Services;
